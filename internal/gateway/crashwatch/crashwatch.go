// Package crashwatch settles servers whose process crashed and stayed
// offline: Wings' crash handler gave up, so the server is stopped like after
// any other stop and its game pod goes (ARCHITECTURE.md 8.3).
package crashwatch

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
)

// Defaults of a Watcher.
const (
	DefaultInterval = 15 * time.Second
	DefaultSettle   = time.Minute
)

// Watcher looks for settled crashes every Interval.
type Watcher struct {
	Store *store.Store
	Log   *slog.Logger
	// Interval between checks; Settle is how long the process must have been
	// offline, and the last power action must be old.
	Interval, Settle time.Duration
	// Now returns the current time; tests substitute a fixed clock.
	Now func() time.Time
}

// Run checks until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check(ctx)
		}
	}
}

// Check sets desired Stopped for every server with a settled crash.
func (w *Watcher) Check(ctx context.Context) {
	list, err := w.Store.List(ctx)
	if err != nil {
		w.Log.Warn("crash check: list failed", "error", err)
		return
	}
	for i := range list {
		gs := &list[i]
		uuid := gs.Spec.Panel.UUID
		if !w.offlineLongEnough(gs) {
			continue
		}
		agent, err := w.Store.AgentPod(ctx, uuid)
		if err != nil {
			continue
		}
		game, err := w.Store.Pod(ctx, uuid)
		if err != nil {
			continue
		}
		if !driven(agent, gs.Status.Agent.PodUID) || !driven(game, gs.Status.Game.PodUID) {
			continue
		}
		if err := w.Store.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": string(v1alpha1.PowerStopped), "kill": false}}); err != nil {
			w.Log.Warn("crash check: stop failed", "uuid", uuid, "error", err)
			continue
		}
		w.Log.Info("process stayed offline after a crash; server stopped", "uuid", uuid)
	}
}

// offlineLongEnough reports a server that should run, whose process has been
// offline for Settle, and whose last power request the operator has acted on
// for at least as long: a start in progress is not a crash.
func (w *Watcher) offlineLongEnough(gs *v1alpha1.GameServer) bool {
	settle := w.Settle
	if settle <= 0 {
		settle = DefaultSettle
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	st, spec := gs.Status, gs.Spec.Power
	if spec.Desired != v1alpha1.PowerRunning || st.Process.State != v1alpha1.ProcessOffline || st.Process.Since == nil {
		return false
	}
	if spec.Generation != st.Power.ObservedGeneration || spec.RestartRequest > st.Power.ObservedRestartRequest {
		return false
	}
	if now.Sub(st.Process.Since.Time) < settle {
		return false
	}
	return st.Power.LastAction == nil || now.Sub(st.Power.LastAction.At.Time) >= settle
}

// driven reports a pod that exists, is the one the operator last drove, and
// is not terminating: a drain or a lost pod is not a crash.
func driven(pod *corev1.Pod, uid string) bool {
	return pod != nil && uid != "" && string(pod.UID) == uid && pod.DeletionTimestamp.IsZero()
}
