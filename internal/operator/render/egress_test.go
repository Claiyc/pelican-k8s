package render

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
)

// A class without blockedEgressCIDRs blocks link-local and the private and
// shared ranges, while the DNS, gateway and game-server allow rules remain.
func TestNetworkPolicyBlocksPrivateRangesByDefault(t *testing.T) {
	np := NetworkPolicy(testInput(t, func(i *Input) { i.Class.Spec.Network = v1alpha1.NetworkSpec{} }))
	want := []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}
	if got := np.Spec.Egress[1].To[0].IPBlock.Except; !reflect.DeepEqual(got, want) {
		t.Fatalf("except %v, want %v", got, want)
	}
	if np.Spec.Egress[0].To[0].NamespaceSelector == nil || len(np.Spec.Egress[0].Ports) != 4 {
		t.Fatalf("dns rule %+v", np.Spec.Egress[0])
	}
	if np.Spec.Egress[2].To[0].PodSelector == nil || np.Spec.Egress[2].Ports[0].Port.IntValue() != GatewayPort {
		t.Fatalf("gateway rule %+v", np.Spec.Egress[2])
	}
	if np.Spec.Egress[3].To[0].PodSelector.MatchLabels[v1alpha1.LabelComponent] != "game" {
		t.Fatalf("game server rule %+v", np.Spec.Egress[3])
	}

	// An explicit empty list blocks only link-local.
	open := NetworkPolicy(testInput(t, func(i *Input) {
		i.Class.Spec.Network = v1alpha1.NetworkSpec{BlockedEgressCIDRs: []string{}}
	}))
	if got := open.Spec.Egress[1].To[0].IPBlock.Except; !reflect.DeepEqual(got, []string{"169.254.0.0/16"}) {
		t.Fatalf("explicit empty list: except %v", got)
	}
}

// The CRD default, the chart's default class (also used by the install-jobs
// policy) and the operator fallback are the same list.
func TestBlockedEgressDefaultsAgree(t *testing.T) {
	var values struct {
		DefaultClass struct {
			Spec struct {
				Network v1alpha1.NetworkSpec `json:"network"`
			} `json:"spec"`
		} `json:"defaultClass"`
	}
	readYAML(t, "../../../charts/pelican-k8s/values.yaml", &values)
	if got := values.DefaultClass.Spec.Network.BlockedEgressCIDRs; !reflect.DeepEqual(got, v1alpha1.DefaultBlockedEgressCIDRs) {
		t.Fatalf("chart values %v, want %v", got, v1alpha1.DefaultBlockedEgressCIDRs)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	readYAML(t, "../../../charts/pelican-k8s/crds/pelican-k8s.io_gameserverclasses.yaml", &crd)
	for _, v := range crd.Spec.Versions {
		def := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["network"].Properties["blockedEgressCIDRs"].Default
		var got []string
		if def != nil {
			if err := json.Unmarshal(def.Raw, &got); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got, v1alpha1.DefaultBlockedEgressCIDRs) {
			t.Fatalf("CRD %s default %v, want %v", v.Name, got, v1alpha1.DefaultBlockedEgressCIDRs)
		}
	}
}

func readYAML(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
