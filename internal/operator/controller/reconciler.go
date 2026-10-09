// Package controller implements the GameServer reconciler (ARCHITECTURE.md 7.6).
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/certs"
	"github.com/Claiyc/pelican-k8s/internal/operator/imageresolve"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
	"github.com/Claiyc/pelican-k8s/internal/pki"
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
	Shim(ctx context.Context) (*agentclient.Shim, error)
	Activity(ctx context.Context) (*agentclient.Activity, error)
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
	// PKI, when set, turns on TLS between the components: every agent gets a
	// certificate Secret, and the operator calls agents over HTTPS with its
	// client certificate through AgentTransport (ARCHITECTURE.md 12.5).
	PKI            *pki.Issuer
	AgentTransport http.RoundTripper
	// CertManager, when set instead of PKI, has cert-manager issue the
	// agents' certificates (ARCHITECTURE.md 12.6); AgentTransport then
	// carries the operator's certificate from its mounted Secret.
	CertManager *certs.CertManager
}

// reader returns Reader, or the client when unset (tests).
func (r *GameServerReconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// tls reports whether traffic between the components runs over TLS.
func (r *GameServerReconciler) tls() bool { return r.PKI != nil || r.CertManager != nil }

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
	// pod is the game pod and agentPod the agent pod; nil when absent.
	pod      *corev1.Pod
	agentPod *corev1.Pod
	// agent drives the agent pod; nil while it is not ready.
	agent AgentAPI
	// state caches the agent's process state for this reconcile.
	state *agentclient.State
	// shim is the agent's view of the shim connection, when asked.
	shim *agentclient.Shim
	// work is the agent's in-flight work (ARCHITECTURE.md 7.7).
	work    []string
	now     metav1.Time
	requeue time.Duration
	// sharedIPElsewhere is the node the game pods sharing the server's
	// address gather on when its own game pod runs on another one.
	sharedIPElsewhere string
	// sharedIPPortsStale is set when the server's game pod shares its address
	// but declares none of the named ports its Service targets.
	sharedIPPortsStale bool
}

// Reconcile implements reconcile.Reconciler.
func (r *GameServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	gs := &v1alpha1.GameServer{}
	if err := r.reader().Get(ctx, req.NamespacedName, gs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	s := &scope{ctx: ctx, gs: gs, orig: gs.DeepCopy(), now: metav1.NewTime(r.now()), requeue: requeueSlow}

	if !gs.DeletionTimestamp.IsZero() {
		return r.finalize(s) //nolint:contextcheck // the scope carries the reconcile context, as for every other step
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
	if gone, err := r.migrateLegacy(s); err != nil || !gone {
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
	if err := r.ensureNetworkPolicies(s); err != nil {
		return err
	}
	if err := r.loadPods(s); err != nil {
		return err
	}
	if err := r.resolveImage(s); err != nil {
		return err
	}
	r.place(s)
	if err := r.ensureAgentStatefulSet(s); err != nil {
		return err
	}
	if err := r.reconcilePods(s); err != nil {
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
	s.in = &render.Input{GS: s.gs, Class: cls, Settings: st, UID: uid, SystemNamespace: r.SystemNamespace, EnvSecretExists: envExists, TLS: r.tls()}
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

// ensureAgentSecret creates the agent's Wings token Secret and the shim
// socket token Secret once; their values never change afterwards.
func (r *GameServerReconciler) ensureAgentSecret(s *scope) error {
	if err := r.ensureSecret(s, names.AgentSecret(s.in.UUID()), func() *corev1.Secret {
		return render.AgentSecret(s.in, randomHex(8), randomHex(32))
	}); err != nil {
		return err
	}
	if err := r.ensureSecret(s, names.ShimSecret(s.in.UUID()), func() *corev1.Secret {
		return render.ShimSecret(s.in, randomHex(32))
	}); err != nil {
		return err
	}
	return r.ensureTLSSecret(s)
}

// ensureTLSSecret issues the agent's certificate and renews it before it
// expires. The agent reads the renewed files from its volume without a
// restart.
func (r *GameServerReconciler) ensureTLSSecret(s *scope) error {
	leaf := certs.Leaf{CommonName: names.AgentService(s.in.UUID()), DNSNames: pki.AgentDNSNames(s.in.UUID(), s.gs.Namespace), Usage: pki.Server}
	if r.CertManager != nil {
		return r.ensureCertificate(s, leaf)
	}
	if r.PKI == nil {
		return nil
	}
	ca, trust := r.PKI.CA(), r.PKI.Trust()
	sec := &corev1.Secret{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: names.TLSSecret(s.in.UUID())}, sec)
	if apierrors.IsNotFound(err) {
		data, _, err := leaf.Data(ca, trust, nil, r.now())
		if err != nil {
			return err
		}
		desired := render.TLSSecret(s.in, data)
		if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(s.ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	data, changed, err := leaf.Data(ca, trust, sec.Data, r.now())
	if err != nil || !changed {
		return err
	}
	sec.Data = data
	if err := r.Update(s.ctx, sec); err != nil {
		return err
	}
	r.event(s, corev1.EventTypeNormal, "CertificateRenewed", "agent certificate renewed")
	return nil
}

// ensureCertificate writes the agent's cert-manager Certificate; cert-manager
// fills the Secret the agent pod mounts and renews it.
func (r *GameServerReconciler) ensureCertificate(s *scope, leaf certs.Leaf) error {
	name := names.TLSSecret(s.in.UUID())
	desired := r.CertManager.Certificate(s.in.Meta(name, "agent"), name, leaf)
	if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certs.CertificateGVK)
	// Uncached: the cache would start an informer for the Certificate kind.
	err := r.reader().Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(s.ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(s.ctx, existing)
}

func (r *GameServerReconciler) ensureSecret(s *scope, name string, build func() *corev1.Secret) error {
	sec := &corev1.Secret{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: name}, sec)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	desired := build()
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
	// NodePort (externalTrafficPolicy Local) and HostPort serve the allocation
	// IP only from the node that owns it: pin the pod there.
	pinProblem := ""
	s.in.NodeNames = nil
	if s.settings.HasAllocation() && pinsToAllocationNode(s.class.Spec.Exposure) {
		nodes := &corev1.NodeList{}
		if err := r.List(s.ctx, nodes); err != nil {
			return err
		}
		s.in.NodeNames, pinProblem = allocationNodes(nodes.Items, s.settings.IPs(), s.class.Spec.Exposure.Mode)
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
			if pinProblem != "" {
				r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionFalse, reasonAllocationIPNotOnNode, pinProblem)
			} else {
				r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionTrue, "HostPort", "")
			}
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
	if pinProblem != "" {
		r.setCondition(s, v1alpha1.ConditionExposureReady, metav1.ConditionFalse, reasonAllocationIPNotOnNode, pinProblem)
		return nil
	}
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
	if serviceType(existing) != serviceType(desired) {
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
	if serviceType(desired) != corev1.ServiceTypeClusterIP && desired.Spec.ClusterIP != corev1.ClusterIPNone {
		existing.Spec.ExternalTrafficPolicy = desired.Spec.ExternalTrafficPolicy
	}
	return r.Patch(s.ctx, existing, patch)
}

// serviceType applies the API server's ClusterIP default, so a rendered Service
// that leaves the type unset matches the stored one.
func serviceType(svc *corev1.Service) corev1.ServiceType {
	if svc.Spec.Type == "" {
		return corev1.ServiceTypeClusterIP
	}
	return svc.Spec.Type
}

func (r *GameServerReconciler) ensureNetworkPolicies(s *scope) error {
	uuid := s.in.UUID()
	if err := r.applyNetworkPolicy(s, names.NetworkPolicy(uuid), render.NetworkPolicy(s.in)); err != nil {
		return err
	}
	return r.applyNetworkPolicy(s, names.AgentNetworkPolicy(uuid), render.AgentNetworkPolicy(s.in))
}

// applyNetworkPolicy creates or updates a policy.
func (r *GameServerReconciler) applyNetworkPolicy(s *scope, name string, desired *networkingv1.NetworkPolicy) error {
	key := types.NamespacedName{Namespace: s.gs.Namespace, Name: name}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(s.ctx, key, existing)
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
	}
	// Offline: a start in progress (the game pod being scheduled, pulled
	// and connected, or the power call not made yet) is Starting.
	if gs.Spec.Power.Desired == v1alpha1.PowerRunning && (gs.Spec.Power.Generation != gs.Status.Power.ObservedGeneration || !condTrue(gs, v1alpha1.ConditionGamePodReady)) {
		return v1alpha1.PhaseStarting
	}
	return v1alpha1.PhaseStopped
}

// quantityString is a small helper for events.
func quantityString(q resource.Quantity) string { return q.String() }

var _ = quantityString
var _ = batchv1.Job{}
