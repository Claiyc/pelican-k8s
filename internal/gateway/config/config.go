// Package config holds the gateway configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the gateway configuration, populated from flags and environment.
type Config struct {
	// PanelURL is the Panel base URL (e.g. https://panel.example.com).
	PanelURL string
	// NodeTokenID and NodeToken are the Panel node's daemon credentials.
	NodeTokenID string
	NodeToken   string
	// ListenPanel serves the Wings API towards the Panel and browsers.
	ListenPanel string
	// ListenRemote serves the remote API towards agents.
	ListenRemote string
	// ListenSFTP serves SFTP towards users.
	ListenSFTP string
	// RemoteURL is how agents reach ListenRemote (their Wings `remote`).
	RemoteURL string
	// ServersNamespace holds GameServers.
	ServersNamespace string
	// SystemNamespace holds the gateway itself (host key Secret).
	SystemNamespace string
	// DefaultClass is written into new GameServers.
	DefaultClass string
	// AdvertisedVersion is the Wings version shown to the Panel.
	AdvertisedVersion string
	// AllowedOrigins are extra websocket origins besides PanelURL.
	AllowedOrigins []string
	// ExternalIPs override the addresses returned by /api/system/ips.
	ExternalIPs []string
	// ResyncInterval is the Panel/CR drift check period.
	ResyncInterval time.Duration
	// SFTPHostKeySecret names the Secret holding the gateway's SSH host key.
	SFTPHostKeySecret string
	// SFTPKeyOnly disables password authentication for SFTP.
	SFTPKeyOnly bool
	// UploadLimitMiB caps browser uploads (must match the agent config and ingress).
	UploadLimitMiB int64
	// TrustedProxies are CIDRs whose X-Forwarded-For is honoured.
	TrustedProxies []string
	// StateCacheTTL bounds how often an agent is polled for GET /api/servers/:s.
	StateCacheTTL time.Duration
	// Timezone is passed to install Jobs as TZ.
	Timezone string
}

// FromEnv builds a Config from environment variables (PELICAN_GW_*).
func FromEnv() (*Config, error) {
	c := &Config{
		PanelURL:          strings.TrimSuffix(os.Getenv("PELICAN_GW_PANEL_URL"), "/"),
		NodeTokenID:       os.Getenv("PELICAN_GW_TOKEN_ID"),
		NodeToken:         os.Getenv("PELICAN_GW_TOKEN"),
		ListenPanel:       envOr("PELICAN_GW_LISTEN_PANEL", ":8080"),
		ListenRemote:      envOr("PELICAN_GW_LISTEN_REMOTE", ":8081"),
		ListenSFTP:        envOr("PELICAN_GW_LISTEN_SFTP", ":2022"),
		RemoteURL:         strings.TrimSuffix(envOr("PELICAN_GW_REMOTE_URL", "http://pelican-gateway.pelican-system.svc:8081"), "/"),
		ServersNamespace:  envOr("PELICAN_SERVERS_NAMESPACE", "pelican-servers"),
		SystemNamespace:   envOr("PELICAN_SYSTEM_NAMESPACE", "pelican-system"),
		DefaultClass:      envOr("PELICAN_DEFAULT_CLASS", "default"),
		AdvertisedVersion: envOr("PELICAN_GW_ADVERTISED_VERSION", "1.0.0"),
		AllowedOrigins:    splitList(os.Getenv("PELICAN_GW_ALLOWED_ORIGINS")),
		ExternalIPs:       splitList(os.Getenv("PELICAN_GW_EXTERNAL_IPS")),
		SFTPHostKeySecret: envOr("PELICAN_GW_SFTP_HOSTKEY_SECRET", "pelican-gateway-sftp-hostkey"),
		SFTPKeyOnly:       os.Getenv("PELICAN_GW_SFTP_KEY_ONLY") == "true",
		TrustedProxies:    splitList(os.Getenv("PELICAN_GW_TRUSTED_PROXIES")),
		Timezone:          envOr("TZ", "UTC"),
		ResyncInterval:    15 * time.Minute,
		StateCacheTTL:     2 * time.Second,
		UploadLimitMiB:    100,
	}
	if v := os.Getenv("PELICAN_GW_RESYNC_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("PELICAN_GW_RESYNC_INTERVAL: %w", err)
		}
		c.ResyncInterval = d
	}
	if v := os.Getenv("PELICAN_GW_UPLOAD_LIMIT_MIB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("PELICAN_GW_UPLOAD_LIMIT_MIB: %w", err)
		}
		c.UploadLimitMiB = n
	}
	// Tokens may be supplied through files (mounted Secrets).
	if c.NodeToken == "" {
		if p := os.Getenv("PELICAN_GW_TOKEN_FILE"); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			c.NodeToken = strings.TrimSpace(string(b))
		}
	}
	if c.NodeTokenID == "" {
		if p := os.Getenv("PELICAN_GW_TOKEN_ID_FILE"); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			c.NodeTokenID = strings.TrimSpace(string(b))
		}
	}
	return c, c.Validate()
}

// Validate checks required fields.
func (c *Config) Validate() error {
	switch {
	case c.PanelURL == "":
		return errors.New("PELICAN_GW_PANEL_URL is required")
	case c.NodeTokenID == "" || c.NodeToken == "":
		return errors.New("PELICAN_GW_TOKEN_ID and PELICAN_GW_TOKEN are required")
	}
	return nil
}

// UserAgent is the Wings User-Agent the Panel expects on every response.
func (c *Config) UserAgent() string {
	return fmt.Sprintf("Pelican Wings/v%s (id:%s)", c.AdvertisedVersion, c.NodeTokenID)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
