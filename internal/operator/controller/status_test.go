package controller

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
)

func TestStatusPatchOnlyChangedFields(t *testing.T) {
	orig := &v1alpha1.GameServerStatus{
		Phase:   v1alpha1.PhaseStopped,
		Process: v1alpha1.ProcessStatus{State: "offline"},
		Install: v1alpha1.InstallStatus{ObservedGeneration: 1, PreparedGeneration: 2, Result: "Running"},
		Agent:   v1alpha1.AgentStatus{PodUID: "a", SftpHostKey: "SHA256:x"},
		Power:   v1alpha1.PowerStatus{ObservedGeneration: 1},
	}
	cur := orig.DeepCopy()
	if p, err := StatusPatch(orig, cur); err != nil || p != nil {
		t.Fatalf("no change expected: %s %v", p, err)
	}
	cur.Phase = v1alpha1.PhaseStarting
	cur.Power.ObservedGeneration = 2
	cur.Power.LastAction = &v1alpha1.PowerActionRecord{Action: "start", At: metav1.Now()}
	cur.Install.RequestedGeneration = 3
	cur.Agent.PodUID = "b"
	cur.Conditions = []metav1.Condition{{Type: "AgentReady", Status: metav1.ConditionTrue, Reason: "Ready"}}
	p, err := StatusPatch(orig, cur)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		t.Fatal(err)
	}
	st := m["status"]
	if st["phase"] != "Starting" {
		t.Fatalf("phase %v", st["phase"])
	}
	if _, ok := st["process"]; ok {
		t.Fatal("untouched process must not be in the patch")
	}
	install := st["install"].(map[string]any)
	if install["requestedGeneration"].(float64) != 3 {
		t.Fatalf("install %v", install)
	}
	if _, ok := install["preparedGeneration"]; ok {
		t.Fatal("gateway-owned preparedGeneration must not be in the patch")
	}
	agent := st["agent"].(map[string]any)
	if agent["podUID"] != "b" {
		t.Fatalf("agent %v", agent)
	}
	if _, ok := agent["sftpHostKey"]; ok {
		t.Fatal("gateway-owned sftpHostKey must not be in the patch")
	}
	power := st["power"].(map[string]any)
	if power["observedGeneration"].(float64) != 2 || power["lastAction"] == nil {
		t.Fatalf("power %v", power)
	}
	if _, ok := st["conditions"]; !ok {
		t.Fatal("conditions expected")
	}
	// Removing a field yields an explicit null.
	cur2 := cur.DeepCopy()
	cur2.Usage = &v1alpha1.UsageStatus{MemoryBytes: 1}
	p2, _ := StatusPatch(cur2, cur)
	json.Unmarshal(p2, &m)
	if v, ok := m["status"]["usage"]; !ok || v != nil {
		t.Fatalf("expected usage: null, got %v", m["status"])
	}
}
