package v1alpha1

import "testing"

func TestBackupsLive(t *testing.T) {
	b := BackupsStatus{Pending: []PendingBackup{{UUID: "a", Agent: "pod-1/0"}, {UUID: "b", Agent: "pod-1/1"}, {UUID: "c"}}}
	if got := b.Live("pod-1/1"); len(got) != 1 || got[0].UUID != "b" {
		t.Fatalf("live = %+v", got)
	}
	// No agent: nothing can finish, not even entries without an agent.
	if got := b.Live(""); len(got) != 0 {
		t.Fatalf("live without agent = %+v", got)
	}
}
