package names

import "testing"

func TestNames(t *testing.T) {
	const u = "0123456789abcdef"
	cases := []struct{ got, want string }{
		{ForUUID(u), "gs-" + u},
		{PVC(u), "gs-" + u},
		{EnvSecret(u), "gs-" + u + "-env"},
		{AgentSecret(u), "gs-" + u + "-agent"},
		{ShimSecret(u), "gs-" + u + "-shim"},
		{TLSSecret(u), "gs-" + u + "-tls"},
		{StatefulSet(u), "gs-" + u},
		{Pod(u), "gs-" + u + "-0"},
		{AgentStatefulSet(u), "gs-" + u + "-agent"},
		{AgentPod(u), "gs-" + u + "-agent-0"},
		{ExposureService(u), "gs-" + u},
		{AgentService(u), "gs-" + u + "-agent"},
		{NetworkPolicy(u), "gs-" + u},
		{AgentNetworkPolicy(u), "gs-" + u + "-agent"},
		{InstallConfigMap(u, 3), "gs-" + u + "-install-3"},
		{InstallJob(u, 3), "gs-" + u + "-install-3"},
		{InstallConfigMap(u, 1234), "gs-" + u + "-install-1234"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q want %q", tc.got, tc.want)
		}
	}
}

func TestUUIDFromName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"gs-abc", "abc", true},
		{ForUUID("u-1"), "u-1", true},
		{"gs-", "", false},
		{"gs", "", false},
		{"xx-abc", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := UUIDFromName(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("UUIDFromName(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestShort(t *testing.T) {
	if got := Short("0123456789"); got != "01234567" {
		t.Fatal(got)
	}
	if got := Short("01234567"); got != "01234567" {
		t.Fatal(got)
	}
	if got := Short("abc"); got != "abc" {
		t.Fatal(got)
	}
}
