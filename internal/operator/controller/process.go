package controller

import (
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// reconcileProcess drives the agent: fresh-pod start, power generations,
// configuration sync and installs (ARCHITECTURE.md 7.6 "Process").
func (r *GameServerReconciler) reconcileProcess(s *scope) error {
	if s.agent == nil {
		return r.reconcileInstall(s)
	}
	gs := s.gs
	recreatePending := condTrue(gs, v1alpha1.ConditionRecreatePending)
	agentUID := string(s.agentPod.UID)
	agentRestarts := render.ContainerRestarts(s.agentPod, render.AgentContainer)
	freshAgent := gs.Status.Agent.PodUID != agentUID || gs.Status.Agent.Restarts != agentRestarts
	gameUID := ""
	if s.pod != nil {
		gameUID = string(s.pod.UID)
	}

	// Fresh pod, or a fresh agent container in the same pod (a node reboot
	// restarts the containers of the pods it keeps): record it and restore the
	// desired state (Wings' "was running before reboot"). With a game pod,
	// wait until its shim is attached, so the start reaches that pod, and
	// until the agent is on the game pod's node, so the start does not precede
	// a move of the agent (section 7.7).
	if freshAgent || gs.Status.Game.PodUID != gameUID {
		if s.pod != nil && (s.shim == nil || !s.shim.Attached || s.shim.PodUID != gameUID || !s.shim.Running && !colocated(s)) {
			s.requeue = requeueFast
			return r.reconcileInstall(s)
		}
		if freshAgent {
			gs.Status.Agent.PodUID, gs.Status.Agent.Restarts = agentUID, agentRestarts
			gs.Status.Agent.SyncedRevision = gs.Spec.Panel.PanelRevision
			gs.Status.Agent.SyncedEnvVersion = r.envSecretVersion(s)
			// An install that was in flight in the previous agent pod lost its
			// agent-side lock and tail; ask the new agent again and start a clean Job.
			if st := &gs.Status.Install; gs.Spec.Install.Generation > st.ObservedGeneration && st.RequestedGeneration != 0 {
				r.event(s, corev1.EventTypeWarning, "InstallRestarted", "agent restarted during install generation %d; requesting it again", gs.Spec.Install.Generation)
				if err := r.deleteInstallJob(s, st.RequestedGeneration); err != nil {
					return err
				}
				st.RequestedGeneration, st.RequestedAt, st.PreparedGeneration, st.JobName = 0, nil, 0, ""
				st.Result = ""
			}
		}
		if gs.Status.Game.PodUID != gameUID {
			gs.Status.Game.PodUID = gameUID
			gs.Status.Agent.RelayedExit = ""
		}
		if s.pod != nil && !s.shim.Running {
			if gs.Spec.Power.Desired == v1alpha1.PowerRunning && !s.settings.Suspended && !recreatePending {
				if err := r.power(s, "start"); err != nil {
					return err
				}
			}
			gs.Status.Power.ObservedGeneration = gs.Spec.Power.Generation
			return r.reconcileInstall(s)
		}
		// A shim that already runs the process (a new agent pod next to a
		// running game pod) is attached by the agent; there is nothing to
		// start. A power generation requested while no agent was ready is
		// applied below.
	}

	// Configuration sync.
	envVersion := r.envSecretVersion(s)
	if gs.Status.Agent.SyncedRevision != gs.Spec.Panel.PanelRevision || gs.Status.Agent.SyncedEnvVersion != envVersion {
		if err := s.agent.Sync(s.ctx, s.in.UUID()); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
		gs.Status.Agent.SyncedRevision = gs.Spec.Panel.PanelRevision
		gs.Status.Agent.SyncedEnvVersion = envVersion
		r.event(s, corev1.EventTypeNormal, "Synced", "agent configuration synced")
	}

	// Power generations.
	if gs.Spec.Power.Generation != gs.Status.Power.ObservedGeneration {
		state, err := r.serverState(s)
		if err != nil {
			return err
		}
		switch gs.Spec.Power.Desired {
		case v1alpha1.PowerRunning:
			if s.settings.Suspended {
				r.event(s, corev1.EventTypeWarning, "Suspended", "start refused: server is suspended")
				gs.Status.Power.ObservedGeneration = gs.Spec.Power.Generation
				break
			}
			if s.pod == nil || !s.pod.DeletionTimestamp.IsZero() || !colocated(s) {
				// The game StatefulSet creates the game pod; the fresh-pod rule
				// starts the process once its shim is attached. An agent on
				// another node moves first.
				s.requeue = requeueFast
				break
			}
			if recreatePending {
				// Stop first; reconcilePods recreates the pod once offline and the
				// fresh-pod rule starts it. The generation stays unobserved until then.
				if state.State != v1alpha1.ProcessOffline {
					if err := r.power(s, "stop"); err != nil {
						return err
					}
				}
				s.requeue = requeueFast
				break
			}
			action := "start"
			if state.State != v1alpha1.ProcessOffline {
				action = "restart"
			}
			if err := r.power(s, action); err != nil {
				return err
			}
			gs.Status.Power.ObservedGeneration = gs.Spec.Power.Generation
		default:
			action := "stop"
			if gs.Spec.Power.Kill {
				action = "kill"
			}
			if state.State != v1alpha1.ProcessOffline {
				if err := r.power(s, action); err != nil {
					return err
				}
			}
			gs.Status.Power.ObservedGeneration = gs.Spec.Power.Generation
		}
	}
	return r.reconcileInstall(s)
}

func (r *GameServerReconciler) power(s *scope, action string) error {
	if err := s.agent.Power(s.ctx, s.in.UUID(), action); err != nil {
		return fmt.Errorf("power %s: %w", action, err)
	}
	rec := &v1alpha1.PowerActionRecord{Action: action, At: s.now}
	if s.pod != nil {
		rec.PodUID = string(s.pod.UID)
	}
	s.gs.Status.Power.LastAction = rec
	r.event(s, corev1.EventTypeNormal, "Power", "issued %s", action)
	s.requeue = requeueFast
	return nil
}

func (r *GameServerReconciler) envSecretVersion(s *scope) string {
	name := s.gs.Spec.Panel.EnvironmentSecretRef.Name
	if name == "" {
		return ""
	}
	sec := &corev1.Secret{}
	if err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: name}, sec); err != nil {
		return ""
	}
	return sec.ResourceVersion
}

// reconcileInstall runs the install state machine (ARCHITECTURE.md 8.2).
func (r *GameServerReconciler) reconcileInstall(s *scope) error {
	gs := s.gs
	gen := gs.Spec.Install.Generation
	st := &gs.Status.Install
	if gen <= 0 || gen <= st.ObservedGeneration {
		r.setCondition(s, v1alpha1.ConditionInstallPrepared, metav1.ConditionFalse, "Idle", "")
		if gen > 0 {
			r.setCondition(s, v1alpha1.ConditionInstalled, metav1.ConditionTrue, st.Result, "")
		}
		return nil
	}
	s.requeue = requeueFast
	r.setCondition(s, v1alpha1.ConditionInstalled, metav1.ConditionFalse, "InProgress", fmt.Sprintf("install generation %d in progress", gen))

	// A new generation supersedes an older in-flight one.
	if st.RequestedGeneration != 0 && st.RequestedGeneration != gen {
		if err := r.deleteInstallJob(s, st.RequestedGeneration); err != nil {
			return err
		}
		st.RequestedGeneration, st.RequestedAt, st.JobName = 0, nil, ""
	}
	if st.RequestedGeneration != gen {
		if s.agent == nil {
			r.setCondition(s, v1alpha1.ConditionInstallPrepared, metav1.ConditionFalse, "AgentNotReady", "waiting for the agent")
			return nil
		}
		err := s.agent.Install(s.ctx, s.in.UUID(), gs.Spec.Install.Reinstall)
		if errors.Is(err, agentclient.ErrConflict) {
			r.setCondition(s, v1alpha1.ConditionInstallPrepared, metav1.ConditionFalse, "PowerActionRunning", "agent is busy with a power action; retrying")
			return nil
		}
		if err != nil {
			return fmt.Errorf("request install: %w", err)
		}
		st.RequestedGeneration = gen
		st.RequestedAt = &s.now
		st.Result = v1alpha1.InstallRunning
		st.FinishedAt = nil
		st.JobName = ""
		r.event(s, corev1.EventTypeNormal, "InstallRequested", "asked the agent to prepare install generation %d", gen)
		r.setCondition(s, v1alpha1.ConditionInstallPrepared, metav1.ConditionFalse, "Requested", "waiting for the agent to hold the install lock")
		return nil
	}

	prepared := st.PreparedGeneration == gen
	if !prepared {
		timeout := time.Duration(s.class.Spec.Install.PrepareTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		if st.RequestedAt != nil && s.now.Sub(st.RequestedAt.Time) > timeout && st.Result == v1alpha1.InstallRunning {
			st.Result = v1alpha1.InstallFailed
			st.FinishedAt = &s.now
			r.event(s, corev1.EventTypeWarning, "InstallTimeout", "agent did not prepare install generation %d within %s", gen, timeout)
		}
		if st.Result == v1alpha1.InstallFailed {
			st.ObservedGeneration = gen
			r.setCondition(s, v1alpha1.ConditionInstalled, metav1.ConditionFalse, "Failed", "install failed before the job started")
			return nil
		}
		r.setCondition(s, v1alpha1.ConditionInstallPrepared, metav1.ConditionFalse, "Requested", "waiting for the agent to hold the install lock")
		return nil
	}
	r.setCondition(s, v1alpha1.ConditionInstallPrepared, metav1.ConditionTrue, "Prepared", "")

	job := &batchv1.Job{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: gs.Namespace, Name: names.InstallJob(s.in.UUID(), gen)}, job)
	switch {
	case apierrors.IsNotFound(err):
		job = nil
	case err != nil:
		return err
	}

	if job == nil && st.Result == v1alpha1.InstallRunning {
		cm := &corev1.ConfigMap{}
		if err := r.Get(s.ctx, types.NamespacedName{Namespace: gs.Namespace, Name: gs.Spec.Install.ScriptConfigMap}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				r.event(s, corev1.EventTypeWarning, "InstallScriptMissing", "ConfigMap %s not found", gs.Spec.Install.ScriptConfigMap)
				return nil
			}
			return err
		}
		desired := render.InstallJob(s.in, gen)
		if err := controllerutil.SetControllerReference(gs, desired, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(s.ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create install job: %w", err)
		}
		st.JobName = desired.Name
		r.event(s, corev1.EventTypeNormal, "InstallStarted", "created install job %s", desired.Name)
		return nil
	}

	jobDone := false
	if job != nil {
		st.JobName = job.Name
		for _, c := range job.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
				jobDone = true
			case batchv1.JobFailed:
				jobDone = true
				if st.Result == v1alpha1.InstallRunning {
					st.Result = v1alpha1.InstallFailed
					st.FinishedAt = &s.now
					r.event(s, corev1.EventTypeWarning, "InstallFailed", "install job failed: %s", c.Message)
				}
			}
		}
	}

	// The gateway records Succeeded/Failed when the agent reports; the Job
	// outcome alone only marks failures. Finish once both sides are done.
	if st.Result == v1alpha1.InstallSucceeded || st.Result == v1alpha1.InstallFailed {
		if job == nil || jobDone {
			st.ObservedGeneration = gen
			// Successful Jobs are removed right away; failed ones stay until their
			// TTL so their pod logs can be inspected.
			if job != nil && st.Result == v1alpha1.InstallSucceeded {
				if err := r.deleteInstallJob(s, gen); err != nil {
					return err
				}
			}
			r.setCondition(s, v1alpha1.ConditionInstalled, metav1.ConditionTrue, st.Result, "")
			r.event(s, corev1.EventTypeNormal, "InstallFinished", "install generation %d finished: %s", gen, st.Result)
		}
	}
	return nil
}

func (r *GameServerReconciler) deleteInstallJob(s *scope, gen int64) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: s.gs.Namespace, Name: names.InstallJob(s.in.UUID(), gen)}}
	policy := metav1.DeletePropagationBackground
	err := r.Delete(s.ctx, job, &client.DeleteOptions{PropagationPolicy: &policy})
	return client.IgnoreNotFound(err)
}

// colocated reports whether the agent pod runs on the game pod's node.
func colocated(s *scope) bool {
	return s.pod != nil && s.agentPod != nil && s.pod.Spec.NodeName == s.agentPod.Spec.NodeName
}
