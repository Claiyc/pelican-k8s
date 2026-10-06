package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// setBase sets the minimum valid environment.
func setBase(t *testing.T) {
	t.Helper()
	t.Setenv("PELICAN_GW_PANEL_URL", "https://panel.example.com/")
	t.Setenv("PELICAN_GW_TOKEN_ID", "id1")
	t.Setenv("PELICAN_GW_TOKEN", "secret")
}

func TestFromEnvDefaults(t *testing.T) {
	setBase(t)
	t.Setenv("TZ", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.PanelURL != "https://panel.example.com" {
		t.Errorf("trailing slash not trimmed: %q", c.PanelURL)
	}
	if c.ListenPanel != ":8080" || c.ListenRemote != ":8081" || c.ListenSFTP != ":2022" {
		t.Errorf("listen defaults: %+v", c)
	}
	if c.ServersNamespace != "pelican-servers" || c.SystemNamespace != "pelican-system" || c.DefaultClass != "default" {
		t.Errorf("namespace defaults: %+v", c)
	}
	if c.Timezone != "UTC" || c.ResyncInterval != 15*time.Minute || c.StateCacheTTL != 2*time.Second {
		t.Errorf("misc defaults: %+v", c)
	}
	if c.MetalLBPools || c.SFTPKeyOnly || c.AllowedOrigins != nil || c.ExternalIPs != nil {
		t.Errorf("flags should default off: %+v", c)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	setBase(t)
	t.Setenv("PELICAN_GW_REMOTE_URL", "http://gw:8081/")
	t.Setenv("PELICAN_GW_ALLOWED_ORIGINS", "https://a.example, ,https://b.example")
	t.Setenv("PELICAN_GW_EXTERNAL_IPS", "192.0.2.1,192.0.2.2")
	t.Setenv("PELICAN_GW_METALLB_POOLS", "true")
	t.Setenv("PELICAN_GW_METALLB_POOL_NAMES", "p1,p2")
	t.Setenv("PELICAN_GW_METALLB_MAX_ADDRESSES", "42")
	t.Setenv("PELICAN_GW_SFTP_KEY_ONLY", "true")
	t.Setenv("PELICAN_GW_RESYNC_INTERVAL", "30s")
	t.Setenv("TZ", "Europe/Berlin")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.RemoteURL != "http://gw:8081" {
		t.Errorf("RemoteURL = %q", c.RemoteURL)
	}
	if !reflect.DeepEqual(c.AllowedOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("AllowedOrigins = %v", c.AllowedOrigins)
	}
	if !reflect.DeepEqual(c.ExternalIPs, []string{"192.0.2.1", "192.0.2.2"}) {
		t.Errorf("ExternalIPs = %v", c.ExternalIPs)
	}
	if !c.MetalLBPools || !reflect.DeepEqual(c.MetalLBPoolNames, []string{"p1", "p2"}) || c.MetalLBMaxAddresses != 42 {
		t.Errorf("metallb settings: %+v", c)
	}
	if !c.SFTPKeyOnly || c.ResyncInterval != 30*time.Second || c.Timezone != "Europe/Berlin" {
		t.Errorf("misc: %+v", c)
	}
}

func TestFromEnvInvalidValues(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"PELICAN_GW_RESYNC_INTERVAL", "soon"},
		{"PELICAN_GW_METALLB_MAX_ADDRESSES", "many"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			setBase(t)
			t.Setenv(tc.key, tc.val)
			_, err := FromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want mention of %s", err, tc.key)
			}
		})
	}
}

func TestFromEnvTokenFiles(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	idFile := filepath.Join(dir, "id")
	if err := os.WriteFile(tokFile, []byte("  filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idFile, []byte("fileid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("read from files", func(t *testing.T) {
		t.Setenv("PELICAN_GW_PANEL_URL", "https://panel.example.com")
		t.Setenv("PELICAN_GW_TOKEN", "")
		t.Setenv("PELICAN_GW_TOKEN_ID", "")
		t.Setenv("PELICAN_GW_TOKEN_FILE", tokFile)
		t.Setenv("PELICAN_GW_TOKEN_ID_FILE", idFile)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.NodeToken != "filetoken" || c.NodeTokenID != "fileid" {
			t.Errorf("token/id = %q/%q", c.NodeToken, c.NodeTokenID)
		}
	})

	t.Run("env wins over file", func(t *testing.T) {
		setBase(t)
		t.Setenv("PELICAN_GW_TOKEN_FILE", tokFile)
		t.Setenv("PELICAN_GW_TOKEN_ID_FILE", idFile)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.NodeToken != "secret" || c.NodeTokenID != "id1" {
			t.Errorf("token/id = %q/%q", c.NodeToken, c.NodeTokenID)
		}
	})

	t.Run("missing token file", func(t *testing.T) {
		setBase(t)
		t.Setenv("PELICAN_GW_TOKEN", "")
		t.Setenv("PELICAN_GW_TOKEN_FILE", filepath.Join(dir, "nope"))
		if _, err := FromEnv(); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("missing token id file", func(t *testing.T) {
		setBase(t)
		t.Setenv("PELICAN_GW_TOKEN_ID", "")
		t.Setenv("PELICAN_GW_TOKEN_ID_FILE", filepath.Join(dir, "nope"))
		if _, err := FromEnv(); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"ok", Config{PanelURL: "u", NodeTokenID: "i", NodeToken: "t"}, ""},
		{"no panel", Config{NodeTokenID: "i", NodeToken: "t"}, "PELICAN_GW_PANEL_URL"},
		{"no token", Config{PanelURL: "u", NodeTokenID: "i"}, "PELICAN_GW_TOKEN"},
		{"no id", Config{PanelURL: "u", NodeToken: "t"}, "PELICAN_GW_TOKEN_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFromEnvValidates(t *testing.T) {
	t.Setenv("PELICAN_GW_PANEL_URL", "")
	t.Setenv("PELICAN_GW_TOKEN", "")
	t.Setenv("PELICAN_GW_TOKEN_ID", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("want validation error")
	}
}

func TestUserAgent(t *testing.T) {
	c := &Config{AdvertisedVersion: "1.2.3", NodeTokenID: "abc"}
	if got, want := c.UserAgent(), "Pelican Wings/v1.2.3 (id:abc)"; got != want {
		t.Errorf("UserAgent = %q, want %q", got, want)
	}
}
