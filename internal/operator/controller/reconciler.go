// Package controller implements the GameServer reconciler (ARCHITECTURE.md 7.6).
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/imageresolve"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
)

// AgentAPI is the subset of the agent client the reconciler uses.
type AgentAPI interface {
	Healthy(ctx context.Context) bool
	GetServer(ctx context.Context, uuid string) (*agentclient.State, error)
	Power(ctx context.Context, uuid, action string) error
	Sync(ctx context.Context, uuid string) error
	Install(ctx context.Context, uuid string, reinstall bool) error
	Delete(ctx context.Context, uuid string) error
	ExitState(ctx context.Context, code int32, oomKilled bool) error
}

// Resolver resolves image digests and entrypoints.
type Resolver interface {
	Digest(ctx context.Context, image string) (string, error)
	Entrypoint(ctx context.Context, image string) ([]string, error)
}

// GameServerReconciler reconciles GameServer objects.
type GameServerReconciler struct {
	client.Client
	// Reader bypasses the informer cache for the GameServer itself so that a
	// reconcile never acts on a status older than the one it just wrote (a
	// stale cache would re-issue power and install requests to the agent).
	Reader          client.Reader
	Recorder        record.EventRecorder
	SystemNamespace string
	Resolver        Resolver
	// NewAgent builds an agent client; tests substitute a fake.
	NewAgent func(base, token string) AgentAPI
	// Now returns the current time; tests substitute a fixed clock.
	Now func() time.Time
	// DefaultClass is used when spec.className is empty.
	DefaultClass string
}

const (
	requeueSlow = 30 * time.Second
	requeueFast = 5 * time.Second
	uidRangeAnn = "openshift.io/sa.scc.uid-range"
)

// errExposureBlocked halts a reconcile that only an operator can unblock. The
// condition already names the cause, so it is not returned to the controller:
// that would bury the message under a generic AgentReady=Error and retry on an
// exponential backoff that cannot help. The regular requeue picks the server up
// again once the class or the allocation changes.
var errExposureBlocked = errors.New("exposure blocked")

// reasonPortOutOfRange marks allocation ports the API server refuses as
// NodePorts. computePhase reports it as Error: without a Service no player can
// reach the server, so it must not look healthy in the Panel.
const reasonPortOutOfRange = "PortOutOfRange"

// scope carries everything a single reconcile needs.
type scope struct {
	ctx      context.Context
	gs       *v1alpha1.GameServer
	orig     *v1alpha1.GameServer
	class    *v1alpha1.GameServerClass
	settings *settings.Settings
	in       *render.Input
	pod      *corev1.Pod
	agent    AgentAPI
	now      metav1.Time
	requeue  time.Duration
}

// Reconcile implements reconcile.Reconciler.
func (r *GameServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	gs := &v1alpha1.GameServer{}
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, req.NamespacedName, gs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	s := &scope{ctx: ctx, gs: gs, orig: gs.DeepCopy(), now: metav1.NewTime(r.now()), requeue: requeueSlow}

	if !gs.DeletionTimestamp.IsZero() {
		return r.finalize(s)
	}
	if !controllerutil.ContainsFinalizer(gs, v1alpha1.Finalizer) {
		controllerutil.AddFinalizer(gs, v1alpha1.Finalizer)
		if err := r.Update(ctx, gs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	err := r.reconcile(s)
	if errors.Is(err, errExposureBlocked) {
		err = nil
	} else if err != nil {
		logger.Error(err, "reconcile failed")
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "Error", truncate(err.Error()))
	}
	s.gs.Status.ObservedGeneration = s.gs.Generation
	s.gs.Status.Phase = computePhase(s)
	if uerr := r.updateStatus(s); uerr != nil {
		if apierrors.IsConflict(uerr) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, uerr
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: s.requeue}, nil
}

func (r *GameServerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *GameServerReconciler) reconcile(s *scope) error {
	if err := r.loadClassAndSettings(s); err != nil {
		return err
	}
	if err := r.ensureAgentSecret(s); err != nil {
		return err
	}
	if err := r.ensurePVC(s); err != nil {
		return err
	}
	if err := r.ensureServices(s); err != nil {
		return err
	}
	if err := r.ensureNetworkPolicy(s); err != nil {
		return err
	}
	if err := r.loadPod(s); err != nil {
		return err
	}
	if err := r.resolveImage(s); err != nil {
		return err
	}
	if err := r.ensureStatefulSet(s); err != nil {
		return err
	}
	if err := r.reconcilePod(s); err != nil {
		return err
	}
	if err := r.reconcileProcess(s); err != nil {
		return err
	}
	if err := r.reconcileSnapshots(s); err != nil {
		return err
	}
	return nil
}

func (r *GameServerReconciler) loadClassAndSettings(s *scope) error {
	name := s.gs.Spec.ClassName
	if name == "" {
		name = r.DefaultClass
	}
	if name == "" {
		name = "default"
	}
	cls := &v1alpha1.GameServerClass{}
	if err := r.Get(s.ctx, types.NamespacedName{Name: name}, cls); err != nil {
		if apierrors.IsNotFound(err) {
			r.event(s, corev1.EventTypeWarning, "ClassNotFound", "GameServerClass %q does not exist", name)
			return fmt.Errorf("GameServerClass %q not found", name)
		}
		return err
	}
	st, err := settings.Parse(s.gs.Spec.Panel.Settings)
	if err != nil {
		return err
	}
	if st.UUID != s.gs.Spec.Panel.UUID {
		return fmt.Errorf("settings uuid %q does not match spec.panel.uuid %q", st.UUID, s.gs.Spec.Panel.UUID)
	}
	s.class = cls
	s.settings = st
	uid, err := r.resolveUID(s)
	if err != nil {
		return err
	}
	envExists := true
	if s.gs.Spec.Panel.EnvironmentSecretRef.Name != "" {
		sec := &corev1.Secret{}
		if err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: s.gs.Spec.Panel.EnvironmentSecretRef.Name}, sec); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			envExists = false
		}
	}
	s.in = &render.Input{GS: s.gs, Class: cls, Settings: st, UID: uid, SystemNamespace: r.SystemNamespace, EnvSecretExists: envExists}
	return nil
}

// resolveUID returns the pinned UID, honouring OpenShift namespace ranges.
func (r *GameServerReconciler) resolveUID(s *scope) (int64, error) {
	uid := s.class.Spec.Security.RunAsUser
	if uid <= 0 {
		uid = 1000
	}
	if !s.class.Spec.Security.UseNamespaceUIDRange {
		return uid, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(s.ctx, types.NamespacedName{Name: s.gs.Namespace}, ns); err != nil {
		return 0, err
	}
	rng := ns.Annotations[uidRangeAnn]
	if rng == "" {
		return uid, nil
	}
	start, _, _ := strings.Cut(rng, "/")
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", uidRangeAnn, rng, err)
	}
	return n, nil
}

func (r *GameServerReconciler) ensureAgentSecret(s *scope) error {
	sec := &corev1.Secret{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: names.AgentSecret(s.in.UUID())}, sec)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	id, token := randomHex(8), randomHex(32)
	desired := render.AgentSecret(s.in, id, token)
	if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
		return err
	}
	if err := r.Create(s.ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (r *GameServerReconciler) ensurePVC(s *scope) error {
	desired := render.PVC(s.in)
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(s.ctx, client.ObjectKeyFromObject(desired), pvc)
	if apierrors.IsNotFound(err) {
		if s.class.Spec.Storage.DeletionPolicy != v1alpha1.DeletionRetain {
			if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
				return err
			}
		}
		if err := r.Create(s.ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		r.setCondition(s, v1alpha1.ConditionVolumeReady, metav1.ConditionFalse, "Provisioning", "volume claim created")
		s.requeue = requeueFast
		return nil
	}
	if err != nil {
		return err
	}
	want := desired.Spec.Resources.Requests[corev1.ResourceStorage]
	have := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	switch want.Cmp(have) {
	case 1:
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		if err := r.Patch(s.ctx, pvc, patch); err != nil {
			r.event(s, corev1.EventTypeWarning, "ResizeFailed", "cannot expand volume to %s: %v", want.String(), err)
		} else {
			r.event(s, corev1.EventTypeNormal, "VolumeExpanded", "volume claim expanded to %s", want.String())
		}
		r.setCondition(s, v1alpha1.ConditionDiskShrink, metav1.ConditionFalse, "Expanded", "")
	case -1:
		r.setCondition(s, v1alpha1.ConditionDiskShrink, metav1.ConditionTrue, "ShrinkRefused", fmt.Sprintf("Panel disk_space asks for %s but the volume is %s; volumes cannot shrink", want.String(), have.String()))
	default:
		r.setCondition(s, v1alpha1.ConditionDiskShrink, metav1.ConditionFalse, "Matches", "")
	}
	if pvc.Status.Phase == corev1.ClaimBound {
		r.setCondition(s, v1alpha1.ConditionVolumeReady, metav1.ConditionTrue, "Bound", "")
	} else {
		r.setCondition(s, v1alpha1.ConditionVolumeReady, metav1.ConditionFalse, string(pvc.Status.Phase), "volume claim is not bound")
		s.requeue = requeueFast
	}
	return nil
}

func (r *GameServerReconciler) ensureServices(s *scope) error {
	agentSvc := render.AgentService(s.in)
	if err := r.applyService(s, agentSvc); err != nil {
		return err
	}
	exposure := render.ExposureService(s.in)
	key := types.NamespacedName{Namespace: s.gs.Namespace, Name: names.ExposureService(s.in.UUID())}
	if exposure == nil {
		existing := &corev1.Service{}
		if err := r.Get(s.ctx, key, existing); err == nil {
			if err := r.Delete(s.ctx, existing); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		s.gs.Status.Endpoints = nil
		if s.class.Spec.Exposure.Mode == v1alpha1.ExposureHostPort && s.settings.HasAllocation() {
			r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionTrue, "HostPort", "")
			s.gs.Status.Endpoints = endpointsFor(s.class.Spec.Exposure.ExternalIPs, s.settings)
		} else {
			r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionTrue, "NoAllocation", "server has no allocation")
		}
		return nil
	}
	if err := r.applyService(s, exposure); err != nil {
		// The API server owns --service-node-port-range, so let it judge the
		// ports and only classify the rejection here: a hardcoded range would
		// block allocations that a widened range accepts.
		if s.class.Spec.Exposure.Mode == v1alpha1.ExposureNodePort && apierrors.IsInvalid(err) {
			if bad := portsOutsideDefaultNodePortRange(s.settings.Ports()); len(bad) > 0 {
				r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionFalse, reasonPortOutOfRange,
					fmt.Sprintf("NodePort exposure needs allocation ports in the NodePort range; %s %s not (widen the API server's --service-node-port-range, move the allocation, or use LoadBalancer or HostPort exposure)",
						joinPorts(bad), plural(len(bad), "is", "are")))
				s.gs.Status.Endpoints = nil
				return errExposureBlocked
			}
		}
		r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionFalse, "ServiceError", truncate(err.Error()))
		return err
	}
	existing := &corev1.Service{}
	if err := r.Get(s.ctx, key, existing); err != nil {
		return err
	}
	ips := s.class.Spec.Exposure.ExternalIPs
	if existing.Spec.Type == corev1.ServiceTypeLoadBalancer {
		ips = nil
		for _, ing := range existing.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				ips = append(ips, ing.IP)
			} else if ing.Hostname != "" {
				ips = append(ips, ing.Hostname)
			}
		}
		if len(ips) == 0 {
			r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionFalse, "Pending", pendingMessage(s))
			s.requeue = requeueFast
			return nil
		}
	}
	s.gs.Status.Endpoints = endpointsFor(ips, s.settings)
	r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionTrue, "Ready", "")
	return nil
}

// defaultNodePortLow and defaultNodePortHigh are Kubernetes' default
// --service-node-port-range. They only classify a rejection the API server has
// already made; they never gate a Service the API server would have accepted.
const (
	defaultNodePortLow  = 30000
	defaultNodePortHigh = 32767
)

func portsOutsideDefaultNodePortRange(ports []int32) []int32 {
	var out []int32
	for _, p := range ports {
		if p < defaultNodePortLow || p > defaultNodePortHigh {
			out = append(out, p)
		}
	}
	return out
}

func joinPorts(ports []int32) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(int(p)))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// lbPendingGrace is how long a LoadBalancer may stay without an address before
// the condition stops saying "waiting" and starts naming the likely cause.
// Cloud load balancers routinely take a minute or two; a cluster with no
// implementation at all waits forever, and the two look identical at first.
const lbPendingGrace = 2 * time.Minute

// pendingMessage explains an address that never arrives. "Waiting" is honest
// for the first minutes and useless after that: the common case is a cluster
// with no load balancer implementation, where nothing will ever assign one and
// the reader needs to be told what to do instead.
func pendingMessage(s *scope) string {
	const waiting = "waiting for the LoadBalancer address"
	c := meta.FindStatusCondition(s.gs.Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Pending" {
		return waiting
	}
	if s.now.Sub(c.LastTransitionTime.Time) < lbPendingGrace {
		return waiting
	}
	return "no LoadBalancer address after " + lbPendingGrace.String() +
		"; the cluster may have no load balancer implementation. Install one (MetalLB on bare metal), " +
		"or set the class exposure.mode to NodePort with allocation ports in 30000-32767, or to HostPort"
}

func endpointsFor(ips []string, st *settings.Settings) []v1alpha1.Endpoint {
	var out []v1alpha1.Endpoint
	if len(ips) == 0 {
		ips = st.IPs()
	}
	for _, ip := range ips {
		for _, p := range st.Ports() {
			out = append(out, v1alpha1.Endpoint{IP: ip, Port: p, Protocols: []string{"TCP", "UDP"}})
		}
	}
	return out
}

// applyService creates or updates a Service keeping the immutable fields.
func (r *GameServerReconciler) applyService(s *scope, desired *corev1.Service) error {
	existing := &corev1.Service{}
	err := r.Get(s.ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
			return err
		}
		return client.IgnoreAlreadyExists(r.Create(s.ctx, desired))
	}
	if err != nil {
		return err
	}
	if existing.Spec.Type != desired.Spec.Type {
		// Type changes (class edit) are simplest through recreation.
		if err := r.Delete(s.ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		s.requeue = requeueFast
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Annotations = desired.Annotations
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
	if desired.Spec.Type != corev1.ServiceTypeClusterIP && desired.Spec.ClusterIP != corev1.ClusterIPNone {
		existing.Spec.ExternalTrafficPolicy = desired.Spec.ExternalTrafficPolicy
	}
	return r.Patch(s.ctx, existing, patch)
}

func (r *GameServerReconciler) ensureNetworkPolicy(s *scope) error {
	desired := render.NetworkPolicy(s.in)
	key := types.NamespacedName{Namespace: s.gs.Namespace, Name: names.NetworkPolicy(s.in.UUID())}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(s.ctx, key, existing)
	if desired == nil {
		if err == nil {
			return client.IgnoreNotFound(r.Delete(s.ctx, existing))
		}
		return client.IgnoreNotFound(err)
	}
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
			return err
		}
		return client.IgnoreAlreadyExists(r.Create(s.ctx, desired))
	}
	if err != nil {
		return err
	}
	if render.Hash(existing.Spec) == render.Hash(desired.Spec) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Patch(s.ctx, existing, patch)
}

func (r *GameServerReconciler) loadPod(s *scope) error {
	pod := &corev1.Pod{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: names.Pod(s.in.UUID())}, pod)
	if apierrors.IsNotFound(err) {
		s.pod = nil
		return nil
	}
	if err != nil {
		return err
	}
	s.pod = pod
	return nil
}

// resolveImage decides the argv and the (digest pinned) image reference.
func (r *GameServerReconciler) resolveImage(s *scope) error {
	image, neverPull := s.settings.Image()
	if image == "" {
		return errors.New("settings.container.image is empty")
	}
	s.in.NeverPull = neverPull
	cls := s.class.Spec.ImageResolution

	if argv, ok := imageresolve.MatchOverride(image, cls.EntrypointOverrides); ok {
		s.in.Argv = argv
	} else if cls.RegistryLookup && r.Resolver != nil {
		argv, err := r.Resolver.Entrypoint(s.ctx, image)
		if err != nil {
			r.event(s, corev1.EventTypeWarning, "EntrypointLookupFailed", "registry lookup for %s failed, falling back to the in-image probe: %v", image, err)
		} else {
			s.in.Argv = argv
		}
	}

	pin := (cls.PinDigest == nil || *cls.PinDigest) && !neverPull && r.Resolver != nil
	if !pin {
		s.in.Image = image
		return nil
	}
	// Keep the digest of the running pod unless the tag changed; re-resolve at every recreate.
	if s.pod != nil {
		if i := render.GameContainerIndex(&s.pod.Spec); i >= 0 {
			current := s.pod.Spec.Containers[i].Image
			if base, _, _ := strings.Cut(current, "@"); base == image && strings.Contains(current, "@") {
				s.in.Image = current
				return nil
			}
		}
	}
	digest, err := r.Resolver.Digest(s.ctx, image)
	if err != nil {
		r.event(s, corev1.EventTypeWarning, "DigestLookupFailed", "cannot resolve digest of %s, using the tag: %v", image, err)
		s.in.Image = image
		return nil
	}
	s.in.Image = render.ContainerImageRef(image, digest)
	return nil
}

func (r *GameServerReconciler) ensureStatefulSet(s *scope) error {
	desired := render.StatefulSet(s.in)
	if s.settings.Suspended && s.class.Spec.SuspendScalesToZero {
		desired.Spec.Replicas = new(int32)
	}
	existing := &appsv1.StatefulSet{}
	err := r.Get(s.ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
			return err
		}
		if err := client.IgnoreAlreadyExists(r.Create(s.ctx, desired)); err != nil {
			return err
		}
		s.gs.Status.PodImage = s.in.Image
		s.gs.Status.TemplateHash = desired.Spec.Template.Annotations[render.AnnotationTemplateHash]
		s.requeue = requeueFast
		return nil
	}
	if err != nil {
		return err
	}
	s.gs.Status.TemplateHash = desired.Spec.Template.Annotations[render.AnnotationTemplateHash]
	s.gs.Status.PodImage = s.in.Image
	if render.Hash(existing.Spec.Template) == render.Hash(desired.Spec.Template) && ptrEq(existing.Spec.Replicas, desired.Spec.Replicas) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Labels = desired.Labels
	return r.Patch(s.ctx, existing, patch)
}

func ptrEq(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// limitsRemoved names the container limits the pod carries and the desired
// resources drop. The API server rejects removing a limit through the resize
// subresource, so such a change is only applied by recreating the pod.
func limitsRemoved(want, have corev1.ResourceRequirements) []string {
	var out []string
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if _, ok := have.Limits[name]; !ok {
			continue
		}
		if _, ok := want.Limits[name]; !ok {
			out = append(out, string(name))
		}
	}
	return out
}

// reconcilePod handles recreation, in-place resize, node loss and exit relay.
func (r *GameServerReconciler) reconcilePod(s *scope) error {
	pod := s.pod
	if pod == nil {
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "NoPod", "pod does not exist yet")
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionFalse, "NoPod", "")
		s.gs.Status.Agent.PodUID = ""
		s.requeue = requeueFast
		return nil
	}
	outdated := pod.Annotations[render.AnnotationTemplateHash] != s.gs.Status.TemplateHash
	restartRequested := s.gs.Spec.Power.RestartRequest > s.gs.Status.Power.ObservedRestartRequest
	if outdated || restartRequested {
		reason, msg := "TemplateChanged", "pod template changed; the pod is recreated once the process is offline"
		if restartRequested {
			reason, msg = "RestartRequested", "restart requested; the pod is recreated once the process is offline"
		}
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionTrue, reason, msg)
	} else {
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionFalse, "UpToDate", "")
	}

	// Node loss fencing.
	if !pod.DeletionTimestamp.IsZero() {
		if fd := s.class.Spec.Failover.ForceDeleteAfter; fd != nil && fd.Duration > 0 && pod.Spec.NodeName != "" {
			node := &corev1.Node{}
			if err := r.Get(s.ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err == nil && !nodeReady(node) {
				r.setCondition(s, v1alpha1.ConditionNodeLost, metav1.ConditionTrue, "NodeNotReady", "pod is terminating on a NotReady node")
				if s.now.Sub(pod.DeletionTimestamp.Time) > fd.Duration {
					r.event(s, corev1.EventTypeWarning, "ForceDelete", "force-deleting pod stuck on NotReady node %s", pod.Spec.NodeName)
					grace := int64(0)
					if err := r.Delete(s.ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
						return err
					}
				}
			}
		}
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "Terminating", "pod is terminating")
		s.requeue = requeueFast
		return nil
	}
	r.setCondition(s, v1alpha1.ConditionNodeLost, metav1.ConditionFalse, "NodeReady", "")

	// In-place resize of the game container.
	resizeRecreate := false
	if i := render.GameContainerIndex(&pod.Spec); i >= 0 {
		want := render.GameResources(s.settings.Build, s.class.Spec.Resources)
		have := pod.Spec.Containers[i].Resources
		if !resourcesEqual(want, have) {
			// Kubernetes refuses to remove a container limit in place, so a
			// Panel change to "unlimited" can only land on a fresh pod.
			// Attempting the resize would fail on every reconcile forever.
			if removed := limitsRemoved(want, have); len(removed) > 0 {
				resizeRecreate = true
				r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionTrue, "RecreateRequired",
					fmt.Sprintf("removing the %s limit needs a new pod; it is applied once the process is offline", strings.Join(removed, " and ")))
				r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionTrue, "ResizeNeedsRecreate",
					"resources cannot be applied in place; the pod is recreated once the process is offline")
			} else {
				resized := pod.DeepCopy()
				resized.Spec.Containers[i].Resources = want
				if err := r.SubResource("resize").Update(s.ctx, resized); err != nil {
					r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionTrue, "ResizeFailed", truncate(err.Error()))
				} else {
					r.event(s, corev1.EventTypeNormal, "Resized", "applied in-place resource change")
					r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionFalse, "Applied", "")
				}
			}
		} else if deferred := resizeDeferred(pod); deferred != "" {
			r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionTrue, deferred, "in-place resize is pending; it is applied at the next pod recreate")
		} else {
			r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionFalse, "Applied", "")
		}
	}
	recreate := outdated || restartRequested || resizeRecreate

	// Agent readiness.
	ready, ip := agentReady(pod)
	if !ready {
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "Starting", "agent container is not ready")
		s.requeue = requeueFast
		// An outdated pod whose game container never started (the agent sidecar
		// gates it) can be recreated without losing a running process.
		if recreate && !gameContainerStarted(pod) {
			r.event(s, corev1.EventTypeNormal, "Recreate", "deleting outdated pod before the game container started")
			if err := r.Delete(s.ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if restartRequested {
				s.gs.Status.Power.ObservedRestartRequest = s.gs.Spec.Power.RestartRequest
			}
			s.pod = nil
		}
		return nil
	}
	token, err := r.agentToken(s)
	if err != nil {
		return err
	}
	s.agent = r.newAgent("http://"+ip+":"+strconv.Itoa(render.AgentPort), token)
	r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionTrue, "Ready", "")

	// Relay a game container termination (e.g. OOM) to the agent.
	if key, code, oom := lastTermination(pod); key != "" && s.gs.Status.Agent.RelayedExit != key {
		if err := s.agent.ExitState(s.ctx, code, oom); err != nil {
			return fmt.Errorf("relay exit state: %w", err)
		}
		s.gs.Status.Agent.RelayedExit = key
		s.gs.Status.Process.LastExit = &v1alpha1.ExitStatus{Code: code, OOMKilled: oom, At: s.now}
		r.event(s, corev1.EventTypeWarning, "GameContainerTerminated", "game container terminated (code %d, oomKilled=%v); relayed to the agent", code, oom)
	}

	// Deferred recreate once the process is offline.
	if recreate {
		state, err := s.agent.GetServer(s.ctx, s.in.UUID())
		if err != nil {
			return err
		}
		if state.State == v1alpha1.ProcessOffline {
			r.event(s, corev1.EventTypeNormal, "Recreate", "deleting pod to apply the new template")
			if err := r.Delete(s.ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if restartRequested {
				s.gs.Status.Power.ObservedRestartRequest = s.gs.Spec.Power.RestartRequest
			}
			s.pod = nil
			s.agent = nil
			s.requeue = requeueFast
		}
	}
	return nil
}

func (r *GameServerReconciler) newAgent(base, token string) AgentAPI {
	if r.NewAgent != nil {
		return r.NewAgent(base, token)
	}
	return agentclient.New(base, token)
}

func (r *GameServerReconciler) agentToken(s *scope) (string, error) {
	sec := &corev1.Secret{}
	if err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: names.AgentSecret(s.in.UUID())}, sec); err != nil {
		return "", err
	}
	return string(sec.Data["token"]), nil
}

func agentReady(pod *corev1.Pod) (bool, string) {
	if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
		return false, ""
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == render.AgentContainer {
			started := cs.Started != nil && *cs.Started
			return started && cs.Ready, pod.Status.PodIP
		}
	}
	return false, ""
}

// lastTermination returns a key for the last termination of the game
// container, or "" when the container never restarted.
func lastTermination(pod *corev1.Pod) (key string, code int32, oom bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != render.GameContainer || cs.LastTerminationState.Terminated == nil {
			continue
		}
		t := cs.LastTerminationState.Terminated
		key = t.ContainerID + "/" + t.FinishedAt.UTC().Format(time.RFC3339)
		return key, t.ExitCode, t.Reason == "OOMKilled"
	}
	return "", 0, false
}

// gameContainerStarted reports whether the game container ever ran in this pod.
func gameContainerStarted(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == render.GameContainer {
			return cs.State.Running != nil || cs.State.Terminated != nil || cs.RestartCount > 0 || cs.LastTerminationState.Terminated != nil
		}
	}
	return false
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func resizeDeferred(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if (c.Type == "PodResizePending" || c.Type == "PodResizeInProgress") && c.Status == corev1.ConditionTrue {
			if c.Reason != "" {
				return c.Reason
			}
			return string(c.Type)
		}
	}
	return ""
}

func resourcesEqual(a, b corev1.ResourceRequirements) bool {
	eq := func(x, y corev1.ResourceList) bool {
		if len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || v.Cmp(w) != 0 {
				return false
			}
		}
		return true
	}
	return eq(a.Requests, b.Requests) && eq(a.Limits, b.Limits)
}

func (r *GameServerReconciler) setCondition(s *scope, t string, status metav1.ConditionStatus, reason, msg string) {
	if reason == "" {
		reason = string(status)
	}
	// Stamp with the reconciler's clock rather than letting meta default to
	// wall time: conditions whose age drives behaviour (see pendingMessage)
	// must move with the same clock the rest of the reconcile uses.
	meta.SetStatusCondition(&s.gs.Status.Conditions, metav1.Condition{Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: s.gs.Generation, LastTransitionTime: s.now})
}

func (r *GameServerReconciler) event(s *scope, typ, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(s.gs, typ, reason, format, args...)
	}
}

func truncate(msg string) string {
	if len(msg) > 512 {
		return msg[:512] + "..."
	}
	return msg
}

func condTrue(gs *v1alpha1.GameServer, t string) bool {
	return meta.IsStatusConditionTrue(gs.Status.Conditions, t)
}

func computePhase(s *scope) v1alpha1.Phase {
	gs := s.gs
	if s.settings != nil && s.settings.Suspended {
		return v1alpha1.PhaseSuspended
	}
	if c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionExposureReady); c != nil && c.Status == metav1.ConditionFalse && c.Reason == reasonPortOutOfRange {
		return v1alpha1.PhaseError
	}
	if c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionAgentReady); c != nil && c.Reason == "Error" {
		return v1alpha1.PhaseError
	}
	if gs.Spec.Install.Generation > gs.Status.Install.ObservedGeneration && gs.Status.Install.Result != v1alpha1.InstallFailed {
		return v1alpha1.PhaseInstalling
	}
	if !condTrue(gs, v1alpha1.ConditionAgentReady) {
		return v1alpha1.PhasePending
	}
	switch gs.Status.Process.State {
	case v1alpha1.ProcessStarting:
		return v1alpha1.PhaseStarting
	case v1alpha1.ProcessRunning:
		return v1alpha1.PhaseRunning
	case v1alpha1.ProcessStopping:
		return v1alpha1.PhaseStopping
	default:
		return v1alpha1.PhaseStopped
	}
}

// quantityString is a small helper for events.
func quantityString(q resource.Quantity) string { return q.String() }

var _ = quantityString
var _ = batchv1.Job{}
