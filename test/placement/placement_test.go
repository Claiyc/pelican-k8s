//go:build placement

// Package placement checks on a live multi-node cluster that the scheduler,
// the StatefulSet controller and kubelet place and move the agent pod and the
// game pod as ARCHITECTURE.md 7.7 describes, and that an open console survives
// the agent pod's replacement (5.9). hack/e2e-placement.sh sets up the
// cluster (kind, three workers, storage not bound to a node), the fake Panel
// and these variables:
//
//	PELICAN_PLACEMENT_GATEWAY  http://127.0.0.1:18080 (the gateway's Panel API)
//	PELICAN_PLACEMENT_TOKEN    node daemon token
//	PELICAN_PLACEMENT_PANEL    http://127.0.0.1:18090 (the fake Panel)
//	PELICAN_PLACEMENT_EGG      the egg image (test/placement/egg)
//
//	go test -tags placement ./test/placement -v -timeout 60m
package placement

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	serversNS = "pelican-servers"
	className = "default"
	// installImage runs the egg's install script. Install containers always
	// pull their image, as Wings does, so it comes from a registry; the egg
	// image is only loaded into the kind nodes.
	installImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
)

type env struct {
	gateway, token, panel, egg string
	http                       *http.Client
	c                          client.Client
	cs                         kubernetes.Interface
	port                       int
}

var shared *env

func load(t *testing.T) *env {
	t.Helper()
	if shared != nil {
		return shared
	}
	e := &env{
		gateway: strings.TrimSuffix(os.Getenv("PELICAN_PLACEMENT_GATEWAY"), "/"),
		token:   os.Getenv("PELICAN_PLACEMENT_TOKEN"),
		panel:   strings.TrimSuffix(os.Getenv("PELICAN_PLACEMENT_PANEL"), "/"),
		egg:     os.Getenv("PELICAN_PLACEMENT_EGG"),
		http:    &http.Client{Timeout: 60 * time.Second},
		port:    30600,
	}
	if e.gateway == "" || e.token == "" || e.panel == "" || e.egg == "" {
		t.Skip("PELICAN_PLACEMENT_* not set")
	}
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	if e.c, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		t.Fatal(err)
	}
	if e.cs, err = kubernetes.NewForConfig(cfg); err != nil {
		t.Fatal(err)
	}
	shared = e
	return e
}

func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// waitFor polls cond until it holds; on timeout the test fails with what cond
// last reported.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, state := cond()
		if state != last {
			t.Logf("%s: %s", what, state)
			last = state
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (last: %s)", timeout, what, last)
		}
		time.Sleep(2 * time.Second)
	}
}

func (e *env) do(t *testing.T, method, base, path, contentType string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// The port-forward reconnects after a reset; retry what never connected.
	for i := 0; ; i++ {
		res, err := e.http.Do(req)
		if err != nil {
			if i < 10 {
				time.Sleep(2 * time.Second)
				if req.GetBody != nil {
					req.Body, _ = req.GetBody()
				}
				continue
			}
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(b)
	}
}

// server is one GameServer of the suite, created through the gateway like the
// Panel does.
type server struct {
	e    *env
	t    *testing.T
	uuid string
}

type serverOpts struct {
	allocationIP string
	crash        bool
}

func (e *env) newServer(t *testing.T, o serverOpts) *server {
	t.Helper()
	uuid := newUUID(t)
	ip := o.allocationIP
	if ip == "" {
		ip = "0.0.0.0"
	}
	e.port++
	settings := map[string]any{
		"id": 1, "uuid": uuid,
		"meta":      map[string]any{"name": t.Name(), "description": ""},
		"suspended": false,
		"environment": map[string]any{
			"STARTUP": "/game.sh", "P_SERVER_UUID": uuid, "P_SERVER_ALLOCATION_LIMIT": 1,
		},
		"invocation":              "/game.sh",
		"skip_egg_scripts":        false,
		"crash_detection_enabled": false,
		"build":                   map[string]any{"memory_limit": 128, "swap": 0, "io_weight": 500, "cpu_limit": 0, "threads": nil, "disk_space": 512, "oom_killer": true},
		"container":               map[string]any{"image": "~" + e.egg, "requires_rebuild": false}, // never pull: the egg exists only on the nodes
		"allocations":             map[string]any{"force_outgoing_ip": false, "default": map[string]any{"ip": ip, "port": e.port}, "mappings": map[string][]int{ip: {e.port}}},
		"egg":                     map[string]any{"id": "5c7f5e0b-0000-4000-8000-000000000001", "file_denylist": []string{}, "features": map[string][]string{}},
		"labels":                  map[string]any{},
		"mounts":                  []any{},
	}
	srv := fakepanel.Server{
		Settings: settings,
		ProcessConfiguration: map[string]any{
			"startup": map[string]any{"done": []string{"Server ready"}, "user_interaction": []string{}, "strip_ansi": false},
			"stop":    map[string]any{"type": "command", "value": "stop"},
			"configs": []any{},
		},
		Install: fakepanel.InstallScript{ContainerImage: installImage, Entrypoint: "sh", Script: "#!/bin/sh\necho installed\n"},
	}
	body, _ := json.Marshal(srv)
	if code, out := e.do(t, http.MethodPut, e.panel, "/_fake/servers/"+uuid, "application/json", strings.NewReader(string(body))); code != http.StatusNoContent {
		t.Fatalf("register server with the fake Panel: %d %s", code, out)
	}
	s := &server{e: e, t: t, uuid: uuid}
	t.Cleanup(s.delete)
	if code, out := e.do(t, http.MethodPost, e.gateway, "/api/servers", "application/json", strings.NewReader(`{"uuid":"`+uuid+`","start_on_completion":false}`)); code != http.StatusAccepted {
		t.Fatalf("create server: %d %s", code, out)
	}
	waitFor(t, 5*time.Minute, "install", func() (bool, string) {
		gs := s.gs()
		if gs == nil {
			return false, "no GameServer"
		}
		if gs.Status.Install.Result == v1alpha1.InstallFailed {
			t.Fatalf("install failed: %+v", gs.Status.Install)
		}
		return gs.Status.Install.Result == "Succeeded", fmt.Sprintf("phase %s, install %q", gs.Status.Phase, gs.Status.Install.Result)
	})
	if o.crash {
		s.writeFile("/crash", "1")
	}
	return s
}

func (s *server) delete() {
	t := s.t
	if t.Failed() {
		s.dump()
	}
	code, out := s.e.do(t, http.MethodDelete, s.e.gateway, "/api/servers/"+s.uuid, "", nil)
	if code != http.StatusNoContent && code != http.StatusNotFound {
		t.Errorf("delete server: %d %s", code, out)
		return
	}
	waitFor(t, 5*time.Minute, "GameServer deletion", func() (bool, string) {
		return s.gs() == nil, "deleting"
	})
}

// dump logs what a failure needs: the GameServer status, both pods and the
// end of their containers' logs.
func (s *server) dump() {
	if gs := s.gs(); gs != nil {
		b, _ := json.MarshalIndent(gs.Status, "", "  ")
		s.t.Logf("status of %s:\n%s", gs.Name, b)
	}
	for _, p := range []*corev1.Pod{s.agentPod(), s.gamePod()} {
		if p == nil {
			continue
		}
		s.t.Logf("pod %s: node %q, phase %s, affinity %+v, conditions %+v", p.Name, p.Spec.NodeName, p.Status.Phase, p.Spec.Affinity, p.Status.Conditions)
		for _, c := range p.Spec.Containers {
			tail := int64(80)
			b, err := s.e.cs.CoreV1().Pods(serversNS).GetLogs(p.Name, &corev1.PodLogOptions{Container: c.Name, TailLines: &tail}).DoRaw(context.Background())
			if err != nil {
				s.t.Logf("logs of %s/%s: %v", p.Name, c.Name, err)
				continue
			}
			s.t.Logf("logs of %s/%s:\n%s", p.Name, c.Name, b)
		}
	}
}

func (s *server) gs() *v1alpha1.GameServer {
	s.t.Helper()
	gs := &v1alpha1.GameServer{}
	err := s.e.c.Get(context.Background(), types.NamespacedName{Namespace: serversNS, Name: names.ForUUID(s.uuid)}, gs)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return gs
}

func (s *server) pod(name string) *corev1.Pod {
	s.t.Helper()
	p := &corev1.Pod{}
	err := s.e.c.Get(context.Background(), types.NamespacedName{Namespace: serversNS, Name: name}, p)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return p
}

func (s *server) gamePod() *corev1.Pod  { return s.pod(names.Pod(s.uuid)) }
func (s *server) agentPod() *corev1.Pod { return s.pod(names.AgentPod(s.uuid)) }

func (s *server) condition(t string) *metav1.Condition {
	gs := s.gs()
	if gs == nil {
		return nil
	}
	return meta.FindStatusCondition(gs.Status.Conditions, t)
}

func (s *server) power(action string) {
	s.t.Helper()
	if code, out := s.e.do(s.t, http.MethodPost, s.e.gateway, "/api/servers/"+s.uuid+"/power", "application/json", strings.NewReader(`{"action":"`+action+`"}`)); code != http.StatusAccepted {
		s.t.Fatalf("power %s: %d %s", action, code, out)
	}
}

func (s *server) readFile(path string) string {
	s.t.Helper()
	code, out := s.e.do(s.t, http.MethodGet, s.e.gateway, "/api/servers/"+s.uuid+"/files/contents?file="+url.QueryEscape(path), "", nil)
	if code == http.StatusNotFound {
		return ""
	}
	if code != http.StatusOK {
		s.t.Fatalf("read %s: %d %s", path, code, out)
	}
	return out
}

func (s *server) writeFile(path, content string) {
	s.t.Helper()
	if code, out := s.e.do(s.t, http.MethodPost, s.e.gateway, "/api/servers/"+s.uuid+"/files/write?file="+url.QueryEscape(path), "text/plain", strings.NewReader(content)); code != http.StatusNoContent && code != http.StatusOK {
		s.t.Fatalf("write %s: %d %s", path, code, out)
	}
}

// lines counts the lines of a file the game writes (starts, stops).
func (s *server) lines(path string) int {
	return len(strings.Fields(s.readFile(path)))
}

// waitAgent waits for a running, ready agent pod and returns it.
func (s *server) waitAgent() *corev1.Pod {
	s.t.Helper()
	var pod *corev1.Pod
	waitFor(s.t, 3*time.Minute, "agent pod ready", func() (bool, string) {
		pod = s.agentPod()
		if pod == nil {
			return false, "no agent pod"
		}
		c := s.condition(v1alpha1.ConditionAgentReady)
		ready := c != nil && c.Status == metav1.ConditionTrue && pod.DeletionTimestamp == nil
		return ready, fmt.Sprintf("%s on %q, phase %s", pod.UID, pod.Spec.NodeName, pod.Status.Phase)
	})
	return pod
}

// waitRunning waits for the process to run in a game pod whose shim is
// attached, and returns the game pod.
func (s *server) waitRunning() *corev1.Pod {
	s.t.Helper()
	var pod *corev1.Pod
	waitFor(s.t, 5*time.Minute, "server running", func() (bool, string) {
		gs := s.gs()
		pod = s.gamePod()
		c := s.condition(v1alpha1.ConditionGamePodReady)
		reason := ""
		if c != nil {
			reason = c.Reason
		}
		node := ""
		if pod != nil {
			node = pod.Spec.NodeName
		}
		ok := gs.Status.Process.State == v1alpha1.ProcessRunning && reason == "Ready" && pod != nil && pod.DeletionTimestamp == nil
		return ok, fmt.Sprintf("phase %s, process %s, GamePodReady %s, game pod on %q", gs.Status.Phase, gs.Status.Process.State, reason, node)
	})
	return pod
}

func (s *server) waitGamePodGone() {
	s.t.Helper()
	waitFor(s.t, 5*time.Minute, "game pod gone", func() (bool, string) {
		gs := s.gs()
		return s.gamePod() == nil, fmt.Sprintf("phase %s, desired %s, process %s", gs.Status.Phase, gs.Spec.Power.Desired, gs.Status.Process.State)
	})
}

func (s *server) sameNode() {
	s.t.Helper()
	a, g := s.agentPod(), s.gamePod()
	if a == nil || g == nil || a.Spec.NodeName == "" || a.Spec.NodeName != g.Spec.NodeName {
		s.t.Fatalf("agent pod and game pod are not on one node: agent %v, game %v", nodeOf(a), nodeOf(g))
	}
}

func nodeOf(p *corev1.Pod) string {
	if p == nil {
		return "<none>"
	}
	return p.Spec.NodeName
}

// workers lists the schedulable worker nodes.
func (e *env) workers(t *testing.T) []corev1.Node {
	t.Helper()
	list := &corev1.NodeList{}
	if err := e.c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	var out []corev1.Node
	for _, n := range list.Items {
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; !cp {
			out = append(out, n)
		}
	}
	if len(out) < 2 {
		t.Fatalf("the suite needs at least two workers, found %d", len(out))
	}
	return out
}

// cordon makes a node unschedulable until the test ends.
func (e *env) cordon(t *testing.T, node string) {
	t.Helper()
	e.setUnschedulable(t, node, true)
	t.Cleanup(func() { e.setUnschedulable(t, node, false) })
}

func (e *env) setUnschedulable(t *testing.T, node string, v bool) {
	t.Helper()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%v}}`, v)
	if err := e.c.Patch(context.Background(), n, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatal(err)
	}
}

// evict evicts a pod the way kubectl drain does.
func (e *env) evict(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
	if err := e.c.SubResource("eviction").Create(context.Background(), pod, ev); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("evict %s: %v", pod.Name, err)
	}
}

func (e *env) deletePod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if err := e.c.Delete(context.Background(), pod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %s: %v", pod.Name, err)
	}
}

func pinnedNode(pod *corev1.Pod) string {
	a := pod.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, m := range term.MatchFields {
			if m.Key == "metadata.name" && len(m.Values) == 1 {
				return m.Values[0]
			}
		}
	}
	return ""
}

func requiresAgentNode(pod *corev1.Pod) bool {
	a := pod.Spec.Affinity
	return a != nil && a.PodAffinity != nil && len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}

// The plain case: the game pod joins the agent, and nothing recreates the agent.
func TestStartStop(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	agent := s.waitAgent()
	s.power("start")
	game := s.waitRunning()
	if game.Spec.NodeName != agent.Spec.NodeName {
		t.Fatalf("game pod on %s, agent on %s", game.Spec.NodeName, agent.Spec.NodeName)
	}
	s.power("stop")
	s.waitGamePodGone()
	if got := s.lines("/stops"); got != 1 {
		t.Fatalf("stops %d", got)
	}
	if a := s.agentPod(); a == nil || a.UID != agent.UID {
		t.Fatal("the agent pod was recreated")
	}
}

// The agent's node takes no new pods: the game pod goes elsewhere, the agent
// follows it, and the process starts once both are on one node.
func TestStartWithAgentNodeFull(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	agent := s.waitAgent()
	e.cordon(t, agent.Spec.NodeName)
	s.power("start")
	relocating := false
	waitFor(t, 5*time.Minute, "agent pod on the game pod's node", func() (bool, string) {
		if c := s.condition(v1alpha1.ConditionAgentRelocating); c != nil && c.Status == metav1.ConditionTrue {
			relocating = true
		}
		a, g := s.agentPod(), s.gamePod()
		ok := a != nil && g != nil && a.UID != agent.UID && g.Spec.NodeName != "" && a.Spec.NodeName == g.Spec.NodeName
		return ok, fmt.Sprintf("agent on %s, game on %s", nodeOf(a), nodeOf(g))
	})
	game := s.waitRunning()
	s.sameNode()
	if game.Spec.NodeName == agent.Spec.NodeName {
		t.Fatalf("the game pod landed on the cordoned node %s", game.Spec.NodeName)
	}
	if !relocating {
		t.Log("AgentRelocating=True was not observed; the move was quicker than the poll")
	}
	if got := s.lines("/starts"); got != 1 {
		t.Fatalf("the process started %d times", got)
	}
}

// The same while the agent has in-flight work: the game pod waits for the
// agent's node and is released when the work ends.
func TestBusyAgentHoldsTheGamePod(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	agent := s.waitAgent()
	// The gateway records SFTP data this way; the operator counts the minute
	// after it as in-flight work.
	busy := func() {
		patch := fmt.Sprintf(`{"status":{"agent":{"sftpActiveAt":%q}}}`, time.Now().UTC().Format(time.RFC3339))
		gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: serversNS, Name: names.ForUUID(s.uuid)}}
		if err := e.c.Status().Patch(context.Background(), gs, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			t.Fatal(err)
		}
	}
	busy()
	e.cordon(t, agent.Spec.NodeName)
	s.power("start")
	waitFor(t, 2*time.Minute, "a pending game pod that requires the agent's node", func() (bool, string) {
		busy()
		g := s.gamePod()
		if g == nil {
			return false, "no game pod"
		}
		if g.Spec.NodeName != "" {
			t.Fatalf("the game pod of a busy agent was scheduled to %s", g.Spec.NodeName)
		}
		c := s.condition(v1alpha1.ConditionGamePodReady)
		return requiresAgentNode(g) && c != nil && c.Reason == "Unschedulable", fmt.Sprintf("required %v, GamePodReady %+v", requiresAgentNode(g), c)
	})
	held := s.gamePod()
	time.Sleep(15 * time.Second)
	if g := s.gamePod(); g == nil || g.UID != held.UID || g.Spec.NodeName != "" {
		t.Fatal("the game pod moved while the agent was busy")
	}
	// The work ends: the next game pod may go anywhere, and the agent follows.
	game := s.waitRunning()
	if game.UID == held.UID {
		t.Fatal("the held game pod was not replaced")
	}
	s.sameNode()
}

// With preferAgentNode off the game pod has no pod affinity at all. The first
// start right after the install may still see the agent's in-flight work
// (which requires the agent's node), so the check is on the game pod of a
// second start.
func TestNoAgentNodePreference(t *testing.T) {
	e := load(t)
	cls := &v1alpha1.GameServerClass{ObjectMeta: metav1.ObjectMeta{Name: className}}
	setPref := func(v bool) {
		patch := fmt.Sprintf(`{"spec":{"scheduling":{"preferAgentNode":%v}}}`, v)
		if err := e.c.Patch(context.Background(), cls, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			t.Fatal(err)
		}
	}
	setPref(false)
	t.Cleanup(func() { setPref(true) })
	s := e.newServer(t, serverOpts{})
	s.waitAgent()
	s.power("start")
	s.waitRunning()
	s.power("stop")
	s.waitGamePodGone()
	s.power("start")
	game := s.waitRunning()
	if a := game.Spec.Affinity; a != nil && a.PodAffinity != nil {
		t.Fatalf("game pod affinity %+v", a.PodAffinity)
	}
	s.sameNode()
}

// A drain of the pair's node: the process gets its stop command, and both pods
// come back on one other node, where the server runs again.
func TestDrain(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	s.waitAgent()
	s.power("start")
	game := s.waitRunning()
	agent := s.agentPod()
	node := game.Spec.NodeName
	e.cordon(t, node)
	e.evict(t, game)
	e.evict(t, agent)
	waitFor(t, 5*time.Minute, "both pods replaced", func() (bool, string) {
		a, g := s.agentPod(), s.gamePod()
		ok := a != nil && g != nil && a.UID != agent.UID && g.UID != game.UID
		return ok, fmt.Sprintf("agent on %s, game on %s", nodeOf(a), nodeOf(g))
	})
	moved := s.waitRunning()
	s.sameNode()
	if moved.Spec.NodeName == node {
		t.Fatalf("the game pod came back on the drained node %s", node)
	}
	if got := s.lines("/stops"); got != 1 {
		t.Fatalf("the process got %d stop commands", got)
	}
	if got := s.lines("/starts"); got != 2 {
		t.Fatalf("the process started %d times", got)
	}
}

// The agent pod goes while the game runs: the process keeps running, and the
// new agent pod on the same node attaches to it.
func TestAgentPodDeletedWhileRunning(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	s.waitAgent()
	s.power("start")
	game := s.waitRunning()
	pid := s.readFile("/pid")
	agent := s.agentPod()
	e.deletePod(t, agent)
	waitFor(t, 3*time.Minute, "a new agent pod", func() (bool, string) {
		a := s.agentPod()
		return a != nil && a.UID != agent.UID && a.Spec.NodeName != "", fmt.Sprintf("agent on %s", nodeOf(a))
	})
	if a := s.agentPod(); a.Spec.NodeName != game.Spec.NodeName || pinnedNode(a) != game.Spec.NodeName {
		t.Fatalf("new agent pod on %s (pinned to %q), game pod on %s", a.Spec.NodeName, pinnedNode(a), game.Spec.NodeName)
	}
	s.waitAgent()
	if g := s.waitRunning(); g.UID != game.UID {
		t.Fatal("the game pod was replaced")
	}
	if got := s.readFile("/pid"); got != pid {
		t.Fatalf("pid %q, was %q", got, pid)
	}
	if got := s.lines("/starts"); got != 1 {
		t.Fatalf("the process started %d times", got)
	}
}

// The agent pod goes while a console is open (ARCHITECTURE.md 5.9): the
// gateway keeps the browser's websocket, asks for a fresh token once the new
// agent is up, and the new agent accepts it.
func TestConsoleSurvivesAgentPodDeletion(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	agent := s.waitAgent()
	c := e.console(t, s.uuid)
	c.auth()
	c.waitFor("auth success")
	e.deletePod(t, agent)
	c.waitFor("token expiring")
	if a := s.agentPod(); a == nil || a.UID == agent.UID {
		t.Fatal("the console was moved before the agent pod was replaced")
	}
	c.auth()
	c.waitFor("auth success")
	c.send("send logs")
}

// The game pod goes while the game runs: the shim stops the process, and the
// server runs again in a new game pod.
func TestGamePodDeletedWhileRunning(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	s.waitAgent()
	s.power("start")
	game := s.waitRunning()
	e.deletePod(t, game)
	waitFor(t, 3*time.Minute, "a new game pod", func() (bool, string) {
		g := s.gamePod()
		return g != nil && g.UID != game.UID, fmt.Sprintf("game pod %v", g != nil)
	})
	s.waitRunning()
	s.sameNode()
	if got := s.lines("/stops"); got != 1 {
		t.Fatalf("the process got %d stop commands", got)
	}
	if got := s.lines("/starts"); got != 2 {
		t.Fatalf("the process started %d times", got)
	}
}

// A process that exits without being restarted: the gateway sets desired
// Stopped after a minute and the game pod goes.
func TestCrashWithoutRestart(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{crash: true})
	s.waitAgent()
	s.power("start")
	waitFor(t, 5*time.Minute, "desired Stopped after the crash", func() (bool, string) {
		gs := s.gs()
		return gs.Spec.Power.Desired == v1alpha1.PowerStopped, fmt.Sprintf("desired %s, process %s", gs.Spec.Power.Desired, gs.Status.Process.State)
	})
	s.waitGamePodGone()
	if got := s.lines("/starts"); got != 1 {
		t.Fatalf("the process started %d times", got)
	}
}

// The agent of a stopped server is not pinned: drained, it goes anywhere.
func TestStoppedServerAgentDrain(t *testing.T) {
	e := load(t)
	s := e.newServer(t, serverOpts{})
	agent := s.waitAgent()
	if got := pinnedNode(agent); got != "" {
		t.Fatalf("the agent of a stopped server is pinned to %s", got)
	}
	e.cordon(t, agent.Spec.NodeName)
	e.evict(t, agent)
	waitFor(t, 3*time.Minute, "agent pod on another node", func() (bool, string) {
		a := s.agentPod()
		ok := a != nil && a.UID != agent.UID && a.Spec.NodeName != "" && a.Spec.NodeName != agent.Spec.NodeName
		return ok, fmt.Sprintf("agent on %s", nodeOf(a))
	})
	s.waitAgent()
}

// NodePort with externalTrafficPolicy Local pins the game pod to the node
// owning the allocation IP; the agent follows.
func TestAllocationIPPinsThePair(t *testing.T) {
	e := load(t)
	workers := e.workers(t)
	target := workers[len(workers)-1]
	ip := ""
	for _, a := range target.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ip = a.Address
		}
	}
	if ip == "" {
		t.Fatalf("node %s has no InternalIP", target.Name)
	}
	s := e.newServer(t, serverOpts{allocationIP: ip})
	s.waitAgent()
	s.power("start")
	game := s.waitRunning()
	if game.Spec.NodeName != target.Name {
		t.Fatalf("game pod on %s, the allocation IP %s is %s's", game.Spec.NodeName, ip, target.Name)
	}
	s.sameNode()
}

// console is a browser's websocket to the gateway.
type console struct {
	t    *testing.T
	e    *env
	uuid string
	c    *websocket.Conn
}

func (e *env) console(t *testing.T, uuid string) *console {
	t.Helper()
	u := strings.Replace(e.gateway, "http://", "ws://", 1) + "/api/servers/" + uuid + "/ws"
	c, res, err := (&websocket.Dialer{HandshakeTimeout: 15 * time.Second}).Dial(u, nil)
	if res != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &console{t: t, e: e, uuid: uuid, c: c}
}

func (c *console) send(event string, args ...string) {
	c.t.Helper()
	if args == nil {
		args = []string{}
	}
	if err := c.c.WriteJSON(map[string]any{"event": event, "args": args}); err != nil {
		c.t.Fatalf("send %s: %v", event, err)
	}
}

// auth sends a fresh token signed with the node token, as the Panel issues it.
func (c *console) auth() {
	c.t.Helper()
	now := time.Now()
	tok, err := jwt.Sign(map[string]any{
		"iss": c.e.panel, "aud": []string{c.e.gateway}, "jti": fmt.Sprintf("placement-%d", now.UnixNano()),
		"iat": now.Unix(), "nbf": now.Add(-5 * time.Minute).Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"server_uuid": c.uuid, "user_uuid": "0f5e4d3c-2b1a-4c9d-8e7f-6a5b4c3d2e1f", "unique_id": fmt.Sprintf("u%d", now.UnixNano()), "scope": "websocket",
		"permissions": []string{"websocket.connect", "control.console"},
	}, jwt.NewHS256([]byte(c.e.token)))
	if err != nil {
		c.t.Fatal(err)
	}
	c.send("auth", string(tok))
}

// waitFor reads frames until one with the event arrives. The gateway holds the
// console for gateway.agentWait (120 s) before it closes it.
func (c *console) waitFor(event string) {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(3 * time.Minute))
	seen := map[string]int{}
	for {
		var m struct {
			Event string   `json:"event"`
			Args  []string `json:"args"`
		}
		if err := c.c.ReadJSON(&m); err != nil {
			c.t.Fatalf("waiting for %s: %v (seen %v)", event, err, seen)
		}
		if m.Event == event {
			return
		}
		if m.Event == "jwt error" || m.Event == "daemon error" {
			c.t.Fatalf("waiting for %s: %s %v", event, m.Event, m.Args)
		}
		seen[m.Event]++
	}
}
