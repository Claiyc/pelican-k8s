package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

func boolPtr(b bool) *bool { return &b }

func TestNetworkPolicyFollowsTheClass(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	var np networkingv1.NetworkPolicy
	if !h.get(&np, names.AgentNetworkPolicy(uuid)) {
		t.Fatal("agent network policy not created")
	}
	if !h.get(&np, names.NetworkPolicy(uuid)) {
		t.Fatal("network policy not created")
	}
	if len(np.OwnerReferences) != 1 {
		t.Fatalf("owner references %v", np.OwnerReferences)
	}
	before := np.ResourceVersion

	// An unchanged class leaves the policy alone.
	h.reconcile(1)
	h.get(&np, names.NetworkPolicy(uuid))
	if np.ResourceVersion != before {
		t.Fatal("an up-to-date policy must not be rewritten")
	}

	// A class change that alters the rules is applied.
	cls := &v1alpha1.GameServerClass{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "default"}, cls); err != nil {
		t.Fatal(err)
	}
	cls.Spec.Network.InClusterEgress.Additional = []v1alpha1.EgressRule{{CIDR: "192.0.2.0/24"}}
	if err := h.c.Update(context.Background(), cls); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	h.get(&np, names.NetworkPolicy(uuid))
	if np.ResourceVersion == before || !strings.Contains(policyCIDRs(&np), "192.0.2.0/24") {
		t.Fatalf("policy not updated: %s", policyCIDRs(&np))
	}

	// Disabling policies removes it, and a missing one is not an error.
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "default"}, cls); err != nil {
		t.Fatal(err)
	}
	cls.Spec.Network.Enabled = boolPtr(false)
	if err := h.c.Update(context.Background(), cls); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if h.get(&np, names.NetworkPolicy(uuid)) || h.get(&np, names.AgentNetworkPolicy(uuid)) {
		t.Fatal("policies must be deleted when the class disables policies")
	}
	h.reconcile(1)
}

func policyCIDRs(np *networkingv1.NetworkPolicy) string {
	var out []string
	for _, r := range np.Spec.Ingress {
		for _, p := range r.From {
			if p.IPBlock != nil {
				out = append(out, p.IPBlock.CIDR)
			}
		}
	}
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.IPBlock != nil {
				out = append(out, p.IPBlock.CIDR)
			}
		}
	}
	return strings.Join(out, ",")
}

type scriptedResolver struct {
	argv       []string
	argvErr    error
	digest     string
	digestErr  error
	digestHits int
	argvHits   int
}

func (r *scriptedResolver) Digest(context.Context, string) (string, error) {
	r.digestHits++
	return r.digest, r.digestErr
}

func (r *scriptedResolver) Entrypoint(context.Context, string) ([]string, error) {
	r.argvHits++
	return r.argv, r.argvErr
}

func resolvedScope(t *testing.T, h *harness) *scope {
	t.Helper()
	return scopeFor(t, h)
}

func TestResolveImage(t *testing.T) {
	const image = "ghcr.io/pelican-eggs/yolks:java_21"
	digest := "sha256:" + strings.Repeat("b", 64)

	t.Run("override wins over registry lookup", func(t *testing.T) {
		cls := newClass()
		cls.Spec.ImageResolution = v1alpha1.ImageResolutionSpec{RegistryLookup: true, EntrypointOverrides: map[string][]string{"ghcr.io/pelican-eggs/yolks:*": {"/tini", "--"}}}
		h := newHarness(t, newGS(), cls)
		res := &scriptedResolver{argv: []string{"/from/registry"}, digest: digest}
		h.r.Resolver = res
		s := resolvedScope(t, h)
		if err := h.r.resolveImage(s); err != nil {
			t.Fatal(err)
		}
		if strings.Join(s.in.Argv, " ") != "/tini --" || res.argvHits != 0 {
			t.Fatalf("argv %v, registry hits %d", s.in.Argv, res.argvHits)
		}
		if s.in.Image != render.ContainerImageRef(image, digest) {
			t.Fatalf("image %q", s.in.Image)
		}
	})

	t.Run("registry lookup supplies the argv", func(t *testing.T) {
		cls := newClass()
		cls.Spec.ImageResolution.RegistryLookup = true
		h := newHarness(t, newGS(), cls)
		h.r.Resolver = &scriptedResolver{argv: []string{"/bin/bash", "/entry.sh"}, digest: digest}
		s := resolvedScope(t, h)
		if err := h.r.resolveImage(s); err != nil {
			t.Fatal(err)
		}
		if strings.Join(s.in.Argv, " ") != "/bin/bash /entry.sh" {
			t.Fatalf("argv %v", s.in.Argv)
		}
	})

	t.Run("failed lookups fall back with a warning", func(t *testing.T) {
		cls := newClass()
		cls.Spec.ImageResolution.RegistryLookup = true
		h := newHarness(t, newGS(), cls)
		h.r.Resolver = &scriptedResolver{argvErr: errors.New("registry down"), digestErr: errors.New("registry down")}
		s := resolvedScope(t, h)
		if err := h.r.resolveImage(s); err != nil {
			t.Fatalf("lookup failures are not fatal: %v", err)
		}
		if s.in.Argv != nil {
			t.Fatalf("argv %v", s.in.Argv)
		}
		if s.in.Image != image {
			t.Fatalf("with no digest the tag is used, got %q", s.in.Image)
		}
		events := recordedEvents(h)
		if !hasEvent(events, "EntrypointLookupFailed") || !hasEvent(events, "DigestLookupFailed") {
			t.Fatalf("events %v", events)
		}
	})

	t.Run("pinning can be disabled", func(t *testing.T) {
		cls := newClass()
		cls.Spec.ImageResolution.PinDigest = boolPtr(false)
		h := newHarness(t, newGS(), cls)
		res := &scriptedResolver{digest: digest}
		h.r.Resolver = res
		s := resolvedScope(t, h)
		if err := h.r.resolveImage(s); err != nil || s.in.Image != image || res.digestHits != 0 {
			t.Fatalf("image %q hits %d err %v", s.in.Image, res.digestHits, err)
		}
	})

	t.Run("no resolver means no pinning", func(t *testing.T) {
		h := newHarness(t, newGS(), newClass())
		h.r.Resolver = nil
		s := resolvedScope(t, h)
		if err := h.r.resolveImage(s); err != nil || s.in.Image != image {
			t.Fatalf("image %q err %v", s.in.Image, err)
		}
	})

	t.Run("a running pod keeps its digest until the tag changes", func(t *testing.T) {
		h := newHarness(t, newGS(), newClass())
		h.reconcile(2)
		h.createPod(true)
		pinned := image + "@" + digest
		pod := h.pod()
		i := render.GameContainerIndex(&pod.Spec)
		pod.Spec.Containers[i].Image = pinned
		if err := h.c.Update(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		res := &scriptedResolver{digest: "sha256:" + strings.Repeat("c", 64)}
		h.r.Resolver = res
		s := resolvedScope(t, h)
		s.pod = h.pod()
		if err := h.r.resolveImage(s); err != nil {
			t.Fatal(err)
		}
		if s.in.Image != pinned || res.digestHits != 0 {
			t.Fatalf("image %q, digest lookups %d", s.in.Image, res.digestHits)
		}

		// A different tag is resolved afresh.
		s.pod.Spec.Containers[i].Image = "ghcr.io/pelican-eggs/yolks:java_17@" + digest
		if err := h.r.resolveImage(s); err != nil {
			t.Fatal(err)
		}
		if s.in.Image != render.ContainerImageRef(image, res.digest) || res.digestHits != 1 {
			t.Fatalf("image %q, digest lookups %d", s.in.Image, res.digestHits)
		}
	})

	t.Run("an empty image is an error", func(t *testing.T) {
		gs := newGS()
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "", false)
		h := newHarness(t, gs, newClass())
		s := resolvedScope(t, h)
		if err := h.r.resolveImage(s); err == nil || !strings.Contains(err.Error(), "image is empty") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestAgentReady(t *testing.T) {
	bt := true
	pod := func(ip string, phase corev1.PodPhase, statuses ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{PodIP: ip, Phase: phase, ContainerStatuses: statuses}}
	}
	agent := func(started *bool, ready bool) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: render.AgentContainer, Started: started, Ready: ready}
	}
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want bool
	}{
		"ready":                 {pod("10.0.0.5", corev1.PodRunning, agent(&bt, true)), true},
		"no ip yet":             {pod("", corev1.PodRunning, agent(&bt, true)), false},
		"pending phase":         {pod("10.0.0.5", corev1.PodPending, agent(&bt, true)), false},
		"started but not ready": {pod("10.0.0.5", corev1.PodRunning, agent(&bt, false)), false},
		"ready but not started": {pod("10.0.0.5", corev1.PodRunning, agent(nil, true)), false},
		"agent status missing":  {pod("10.0.0.5", corev1.PodRunning, corev1.ContainerStatus{Name: "other", Started: &bt, Ready: true}), false},
	} {
		ready, ip := agentReady(tc.pod)
		if ready != tc.want || (ready && ip != "10.0.0.5") {
			t.Errorf("%s: ready=%v ip=%q", name, ready, ip)
		}
	}
}

func TestNewAgentDefaultsToTheHTTPClient(t *testing.T) {
	r := &GameServerReconciler{}
	if _, ok := r.newAgent("http://10.0.0.1:8080", "t").(*agentclient.Client); !ok {
		t.Fatal("without an injected factory the real agent client is used")
	}
	called := ""
	r.NewAgent = func(base, token string) AgentAPI { called = base + "|" + token; return &fakeAgent{} }
	r.newAgent("b", "t")
	if called != "b|t" {
		t.Fatalf("factory not used: %q", called)
	}
}

func TestReconcilerClock(t *testing.T) {
	if (&GameServerReconciler{}).now().IsZero() {
		t.Fatal("the default clock is wall time")
	}
}

func TestRecreateConditionClearsWithoutPod(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionAgentReady); c == nil || c.Reason != "NoPod" {
		t.Fatalf("condition %+v", c)
	}
}
