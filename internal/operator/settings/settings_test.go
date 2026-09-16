package settings

import (
	"encoding/json"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const sample = `{"id":1,"uuid":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","meta":{"name":"Survival SMP","description":""},"suspended":false,
"invocation":"java -jar {{SERVER_JARFILE}}","skip_egg_scripts":false,
"build":{"memory_limit":4096,"swap":0,"io_weight":500,"cpu_limit":200,"threads":null,"disk_space":10240,"oom_killer":true},
"container":{"image":"~ghcr.io/pelican-eggs/yolks:java_21","requires_rebuild":false},
"allocations":{"force_outgoing_ip":false,"default":{"ip":"203.0.113.10","port":25565},"mappings":{"203.0.113.10":[25565,25575],"203.0.113.11":[25565]}},
"egg":{"id":"9f8e","file_denylist":[],"features":{"eula":["You need to agree"]}},"labels":{},"mounts":[]}`

func TestParse(t *testing.T) {
	s, err := Parse(apiextensionsv1.JSON{Raw: []byte(sample)})
	if err != nil {
		t.Fatal(err)
	}
	if s.UUID != "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d" || s.Meta.Name != "Survival SMP" || s.Build.MemoryLimit != 4096 || s.Build.CPULimit != 200 || s.Build.DiskSpace != 10240 {
		t.Fatalf("parsed %+v", s)
	}
	if !s.HasAllocation() {
		t.Fatal("expected allocation")
	}
	if ports := s.Ports(); len(ports) != 2 || ports[0] != 25565 || ports[1] != 25575 {
		t.Fatalf("ports %v", ports)
	}
	if ips := s.IPs(); len(ips) != 2 || ips[0] != "203.0.113.10" {
		t.Fatalf("ips %v", ips)
	}
	img, never := s.Image()
	if img != "ghcr.io/pelican-eggs/yolks:java_21" || !never {
		t.Fatalf("image %q %v", img, never)
	}
	if _, err := Parse(apiextensionsv1.JSON{}); err == nil {
		t.Fatal("empty should fail")
	}
	if _, err := Parse(apiextensionsv1.JSON{Raw: []byte(`{"id":1}`)}); err == nil {
		t.Fatal("missing uuid should fail")
	}
}

func TestNoAllocation(t *testing.T) {
	s, err := Parse(apiextensionsv1.JSON{Raw: []byte(`{"uuid":"x","allocations":{"default":{"ip":"127.0.0.1","port":0},"mappings":{"":[]}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if s.HasAllocation() || len(s.Ports()) != 0 || len(s.IPs()) != 0 {
		t.Fatalf("unexpected allocation %+v", s.Allocations)
	}
}

func TestRewriteForPod(t *testing.T) {
	out, err := RewriteForPod([]byte(sample), "0.0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	def := m["allocations"].(map[string]any)["default"].(map[string]any)
	if def["ip"] != "0.0.0.0" || def["port"].(float64) != 25565 {
		t.Fatalf("default %v", def)
	}
	if m["meta"].(map[string]any)["name"] != "Survival SMP" {
		t.Fatal("other fields must be preserved")
	}
	// No allocation: untouched.
	raw := []byte(`{"uuid":"x","allocations":{"default":{"ip":"127.0.0.1","port":0},"mappings":{"":[]}}}`)
	out, _ = RewriteForPod(raw, "0.0.0.0")
	json.Unmarshal(out, &m)
	if m["allocations"].(map[string]any)["default"].(map[string]any)["ip"] != "127.0.0.1" {
		t.Fatal("servers without allocation keep 127.0.0.1")
	}
}
