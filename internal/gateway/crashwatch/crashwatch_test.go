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

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
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
			Agent:   v1alpha1.AgentStatus{PodUID: "agent-1"},
			Game:    v1alpha1.GameStatus{PodUID: "game-1"},
		},
	}
	agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AgentPod(uuid), UID: "agent-1"}}
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.Pod(uuid), UID: "game-1"}}
	return gs, agent, game
}

func check(t *testing.T, gs *v1alpha1.GameServer, pods ...*corev1.Pod) v1alpha1.PowerState {
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&v1alpha1.GameServer{}).Build()
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
		(&Watcher{Interval: time.Millisecond}).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
