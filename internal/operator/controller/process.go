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
	if s.agent == nil || s.pod == nil {
		if err := r.reconcileInstall(s); err != nil {
			return err
		}
		return nil
	}
	gs := s.gs
	podUID := string(s.pod.UID)
	recreatePending := condTrue(gs, v1alpha1.ConditionRecreatePending)

	// Fresh pod: record it and restore the desired state (Wings' "was running before reboot").
	if gs.Status.Agent.PodUID != podUID {
		gs.Status.Agent.PodUID = podUID
		gs.Status.Agent.SyncedRevision = gs.Spec.Panel.PanelRevision
		gs.Status.Agent.SyncedEnvVersion = r.envSecretVersion(s)
		gs.Status.Agent.RelayedExit = ""
		// An install that was in flight in the previous pod lost its agent-side
		// lock and tail; ask the new agent again and start a clean Job.
		if st := &gs.Status.Install; gs.Spec.Install.Generation > st.ObservedGeneration && st.RequestedGeneration != 0 {
			r.event(s, corev1.EventTypeWarning, "InstallRestarted", "pod was recreated during install generation %d; requesting it again", gs.Spec.Install.Generation)
			if err := r.deleteInstallJob(s, st.RequestedGeneration); err != nil {
				return err
			}
			st.RequestedGeneration, st.RequestedAt, st.PreparedGeneration, st.JobName = 0, nil, 0, ""
			st.Result = ""
		}
		if gs.Spec.Power.Desired == v1alpha1.PowerRunning && !s.settings.Suspended && !recreatePending {
			if err := r.power(s, "start"); err != nil {
				return err
			}
		}
		gs.Status.Power.ObservedGeneration = gs.Spec.Power.Generation
		return r.reconcileInstall(s)
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
		state, err := s.agent.GetServer(s.ctx, s.in.UUID())
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
			if recreatePending {
				// Stop first; reconcilePod recreates the pod once offline and the
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
	s.gs.Status.Power.LastAction = &v1alpha1.PowerActionRecord{Action: action, At: s.now, PodUID: string(s.pod.UID)}
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
		if st.RequestedAt != nil && s.now.Time.Sub(st.RequestedAt.Time) > timeout && st.Result == v1alpha1.InstallRunning {
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
