package crashwatch

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

const (
	ns   = "servers"
	uuid = "11111111-1111-1111-1111-111111111111"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// crashed is a server whose process went offline two minutes ago, with the
// operator caught up and both pods the ones it drove.
func crashed() (*v1alpha1.GameServer, *corev1.Pod, *corev1.Pod) {
	ago := metav1.NewTime(now.Add(-2 * time.Minute))
	gs := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.ForUUID(uuid)},
		Spec: v1alpha1.GameServerSpec{
			Panel: v1alpha1.PanelSpec{UUID: uuid},
			Power: v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 3},
		},
		Status: v1alpha1.GameServerStatus{
			Process: v1alpha1.ProcessStatus{State: v1alpha1.ProcessOffline, Since: &ago},
			Power:   v1alpha1.PowerStatus{ObservedGeneration: 3, LastAction: &v1alpha1.PowerActionRecord{Action: "start", At: metav1.NewTime(now.Add(-10 * time.Minute))}},
			Agent:   v1alpha1.AgentStatus{PodUID: "agent-1", ContainerID: "cri://a"},
			Game:    v1alpha1.GameStatus{PodUID: "game-1"},
		},
	}
	agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AgentPod(uuid), UID: "agent-1"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: render.AgentContainer, ContainerID: "cri://a"}}}}
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.Pod(uuid), UID: "game-1"}}
	return gs, agent, game
}

func check(t *testing.T, gs *v1alpha1.GameServer, pods ...*corev1.Pod) v1alpha1.PowerState {
	t.Helper()
	return checkWith(t, nil, gs, pods...)
}

// checkWith runs the check with funcs intercepting the store's client calls.
func checkWith(t *testing.T, funcs *interceptor.Funcs, gs *v1alpha1.GameServer, pods ...*corev1.Pod) v1alpha1.PowerState {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	objs := []client.Object{gs}
	for _, p := range pods {
		if p != nil {
			objs = append(objs, p)
		}
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&v1alpha1.GameServer{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	w := &Watcher{Store: store.New(c, ns, "default"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	w.Check(context.Background())
	out := &v1alpha1.GameServer{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: gs.Name}, out); err != nil {
		t.Fatal(err)
	}
	return out.Spec.Power.Desired
}

func TestSettledCrashStopsTheServer(t *testing.T) {
	gs, agent, game := crashed()
	if got := check(t, gs, agent, game); got != v1alpha1.PowerStopped {
		t.Fatalf("desired %s, want Stopped", got)
	}
	// An agent container restart the operator has driven is the same agent.
	gs, agent, game = crashed()
	gs.Status.Agent.ContainerID = "cri://b"
	agent.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: render.AgentContainer, ContainerID: "cri://b", RestartCount: 1}}
	if got := check(t, gs, agent, game); got != v1alpha1.PowerStopped {
		t.Fatalf("desired %s after a driven agent restart, want Stopped", got)
	}
}

// A change written after the check read the server wins: the check decides
// again on the newer version. A start another gateway replica writes is kept,
// and so is a server whose agent the operator has just recorded as fresh.
func TestChangeDuringTheCheckIsKept(t *testing.T) {
	cases := map[string]func(ctx context.Context, c client.WithWatch, gs client.Object) error{
		"start on another replica": func(ctx context.Context, c client.WithWatch, gs client.Object) error {
			return c.Patch(ctx, gs, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"power":{"desired":"Running","generation":4}}}`)))
		},
		"fresh agent recorded": func(ctx context.Context, c client.WithWatch, gs client.Object) error {
			return c.Status().Patch(ctx, gs, client.RawPatch(types.MergePatchType, []byte(`{"status":{"agent":{"containerID":"cri://b"}}}`)))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			gs, agent, game := crashed()
			raced := false
			funcs := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if !raced {
					raced = true
					if err := change(ctx, c, obj.DeepCopyObject().(client.Object)); err != nil {
						t.Fatal(err)
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}
			if got := checkWith(t, &funcs, gs, agent, game); got != v1alpha1.PowerRunning || !raced {
				t.Fatalf("desired %s (raced %v), want Running", got, raced)
			}
		})
	}
}

func TestNotASettledCrash(t *testing.T) {
	deleting := metav1.NewTime(now)
	cases := map[string]func(gs *v1alpha1.GameServer, agent, game **corev1.Pod){
		"desired stopped": func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) { gs.Spec.Power.Desired = v1alpha1.PowerStopped },
		"process running": func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) { gs.Status.Process.State = v1alpha1.ProcessRunning },
		"no since":        func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) { gs.Status.Process.Since = nil },
		"offline for a short while": func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) {
			t := metav1.NewTime(now.Add(-30 * time.Second))
			gs.Status.Process.Since = &t
		},
		"power generation pending": func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) { gs.Spec.Power.Generation = 4 },
		"restart request pending":  func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) { gs.Spec.Power.RestartRequest = 1 },
		"start just issued": func(gs *v1alpha1.GameServer, _, _ **corev1.Pod) {
			gs.Status.Power.LastAction.At = metav1.NewTime(now.Add(-10 * time.Second))
		},
		"no game pod":     func(_ *v1alpha1.GameServer, _, game **corev1.Pod) { *game = nil },
		"no agent pod":    func(_ *v1alpha1.GameServer, agent, _ **corev1.Pod) { *agent = nil },
		"fresh game pod":  func(_ *v1alpha1.GameServer, _, game **corev1.Pod) { (*game).UID = "game-2" },
		"fresh agent pod": func(_ *v1alpha1.GameServer, agent, _ **corev1.Pod) { (*agent).UID = "agent-2" },
		"restarted agent container": func(_ *v1alpha1.GameServer, agent, _ **corev1.Pod) {
			(*agent).Status.ContainerStatuses = []corev1.ContainerStatus{{Name: render.AgentContainer, ContainerID: "cri://b"}}
		},
		"agent container not started": func(gs *v1alpha1.GameServer, agent, _ **corev1.Pod) {
			gs.Status.Agent.ContainerID = ""
			(*agent).Status.ContainerStatuses = []corev1.ContainerStatus{{Name: render.AgentContainer}}
		},
		"game pod draining": func(_ *v1alpha1.GameServer, _, game **corev1.Pod) {
			(*game).DeletionTimestamp = &deleting
			(*game).Finalizers = []string{"x"}
		},
		"agent pod lost": func(_ *v1alpha1.GameServer, agent, _ **corev1.Pod) {
			(*agent).DeletionTimestamp = &deleting
			(*agent).Finalizers = []string{"x"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			gs, agent, game := crashed()
			mutate(gs, &agent, &game)
			want := gs.Spec.Power.Desired
			if got := check(t, gs, agent, game); got != want {
				t.Fatalf("desired %s, want %s", got, want)
			}
		})
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Watcher{Interval: time.Hour}).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
