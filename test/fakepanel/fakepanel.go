// Package fakepanel is an in-memory stand-in for the Pelican Panel's remote API
// (/api/remote/*) plus the pelican-k8s gateway extensions. It is used by the
// agent spike and by gateway tests.
package fakepanel

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server is one server known to the fake panel.
type Server struct {
	UUID                 string
	Settings             map[string]any
	ProcessConfiguration map[string]any
	Install              InstallScript
	// State is updated from container/status posts.
	State string
	// Install bookkeeping used by the gateway extension endpoints.
	InstallGeneration int64
	InstallResult     string
	StrictExitCode    bool
}

// InstallScript mirrors the Panel's install payload.
type InstallScript struct {
	ContainerImage string `json:"container_image"`
	Entrypoint     string `json:"entrypoint"`
	Script         string `json:"script"`
}

// Panel is the fake panel state.
type Panel struct {
	mu       sync.Mutex
	TokenID  string
	Token    string
	Servers  map[string]*Server
	Activity []map[string]any
	// Calls records every request as "METHOD /path".
	Calls []string
	// InstallResults records install status posts per server.
	InstallResults map[string][]map[string]any
	// SftpAuth answers SFTP auth requests; nil rejects everything.
	SftpAuth func(req map[string]any) (map[string]any, bool)
	// BackupUploadParts, when set, answers GET /backups/{uuid}.
	BackupUploadParts func(uuid string, size int64) map[string]any
	StateChanges      []map[string]string
}

// New returns an empty fake panel accepting tokenID.token.
func New(tokenID, token string) *Panel {
	return &Panel{TokenID: tokenID, Token: token, Servers: map[string]*Server{}, InstallResults: map[string][]map[string]any{}}
}

// Add registers a server.
func (p *Panel) Add(s *Server) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.State == "" {
		s.State = "offline"
	}
	p.Servers[s.UUID] = s
}

// Get returns a server by UUID.
func (p *Panel) Get(uuid string) *Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Servers[uuid]
}

func (p *Panel) record(r *http.Request) {
	p.mu.Lock()
	p.Calls = append(p.Calls, r.Method+" "+r.URL.Path)
	p.mu.Unlock()
}

// CallsMatching returns recorded calls containing substr.
func (p *Panel) CallsMatching(substr string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.Calls {
		if strings.Contains(c, substr) {
			out = append(out, c)
		}
	}
	return out
}

func (p *Panel) authorized(r *http.Request) bool {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return auth == p.TokenID+"."+p.Token
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Panel) serverPayload(s *Server) map[string]any {
	return map[string]any{"uuid": s.UUID, "settings": s.Settings, "process_configuration": s.ProcessConfiguration}
}

// Handler returns the HTTP handler serving /api/remote/*.
func (p *Panel) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/", func(w http.ResponseWriter, r *http.Request) {
		p.record(r)
		if !p.authorized(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"errors": []map[string]string{{"code": "AccessDeniedHttpException", "status": "403", "detail": "unauthorized"}}})
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/remote")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case path == "/servers" && r.Method == http.MethodGet:
			p.mu.Lock()
			data := make([]map[string]any, 0, len(p.Servers))
			for _, s := range p.Servers {
				data = append(data, p.serverPayload(s))
			}
			p.mu.Unlock()
			writeJSON(w, 200, map[string]any{"data": data, "meta": map[string]any{"current_page": 1, "last_page": 1, "per_page": 50, "total": len(data)}})
		case path == "/servers/reset" && r.Method == http.MethodPost:
			w.WriteHeader(204)
		case path == "/activity" && r.Method == http.MethodPost:
			var body struct {
				Data []map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.mu.Lock()
			p.Activity = append(p.Activity, body.Data...)
			p.mu.Unlock()
			w.WriteHeader(204)
		case path == "/sftp/auth" && r.Method == http.MethodPost:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if p.SftpAuth == nil {
				writeJSON(w, 403, map[string]any{"errors": []map[string]string{{"code": "AccessDeniedHttpException", "status": "403", "detail": "invalid credentials"}}})
				return
			}
			res, ok := p.SftpAuth(req)
			if !ok {
				writeJSON(w, 403, map[string]any{"errors": []map[string]string{{"code": "AccessDeniedHttpException", "status": "403", "detail": "invalid credentials"}}})
				return
			}
			writeJSON(w, 200, res)
		case len(parts) >= 2 && parts[0] == "backups":
			p.backups(w, r, parts[1:])
		case len(parts) >= 2 && parts[0] == "servers":
			p.server(w, r, parts[1], parts[2:])
		default:
			writeJSON(w, 404, map[string]any{"errors": []map[string]string{{"code": "NotFoundHttpException", "status": "404", "detail": "not found"}}})
		}
	})
	return mux
}

func (p *Panel) backups(w http.ResponseWriter, r *http.Request, rest []string) {
	uuid := rest[0]
	switch {
	case len(rest) == 1 && r.Method == http.MethodGet:
		if p.BackupUploadParts == nil {
			writeJSON(w, 404, map[string]any{"errors": []map[string]string{{"code": "NotFound", "status": "404", "detail": "no s3"}}})
			return
		}
		writeJSON(w, 200, p.BackupUploadParts(uuid, 0))
	case len(rest) == 1 && r.Method == http.MethodPost:
		w.WriteHeader(204)
	case len(rest) == 2 && rest[1] == "restore":
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

func (p *Panel) server(w http.ResponseWriter, r *http.Request, uuid string, rest []string) {
	s := p.Get(uuid)
	if s == nil {
		writeJSON(w, 404, map[string]any{"errors": []map[string]string{{"code": "NotFoundHttpException", "status": "404", "detail": "The requested resource does not exist."}}})
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		p.mu.Lock()
		payload := map[string]any{"settings": s.Settings, "process_configuration": s.ProcessConfiguration}
		p.mu.Unlock()
		writeJSON(w, 200, payload)
	case len(rest) == 1 && rest[0] == "install" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.Install)
	case len(rest) == 1 && rest[0] == "install" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.InstallResults[uuid] = append(p.InstallResults[uuid], body)
		if ok, _ := body["successful"].(bool); ok {
			s.InstallResult = "Succeeded"
		} else {
			s.InstallResult = "Failed"
		}
		p.mu.Unlock()
		w.WriteHeader(204)
	case len(rest) == 2 && rest[0] == "container" && rest[1] == "status":
		var body struct {
			Data map[string]string `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		s.State = body.Data["new_state"]
		p.StateChanges = append(p.StateChanges, body.Data)
		p.mu.Unlock()
		w.WriteHeader(204)
	// --- pelican-k8s gateway extensions used by the agent installer ---
	case len(rest) == 2 && rest[0] == "install" && rest[1] == "prepared" && r.Method == http.MethodPost:
		p.mu.Lock()
		if s.InstallGeneration == 0 {
			s.InstallGeneration = 1
		}
		s.InstallResult = "Running"
		out := map[string]any{"generation": s.InstallGeneration, "strict_exit_code": s.StrictExitCode}
		p.mu.Unlock()
		writeJSON(w, 200, out)
	case len(rest) == 2 && rest[0] == "install" && rest[1] == "state":
		p.mu.Lock()
		out := map[string]any{"generation": s.InstallGeneration, "result": s.InstallResult, "job_finished": s.InstallResult != "Running" && s.InstallResult != ""}
		p.mu.Unlock()
		writeJSON(w, 200, out)
	case len(rest) == 2 && rest[0] == "transfer":
		w.WriteHeader(204)
	default:
		writeJSON(w, 404, map[string]any{"errors": []map[string]string{{"code": "NotFoundHttpException", "status": "404", "detail": "not found"}}})
	}
}

// WaitForState blocks until the server reaches state or the timeout passes.
func (p *Panel) WaitForState(uuid, state string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := p.Get(uuid); s != nil {
			p.mu.Lock()
			st := s.State
			p.mu.Unlock()
			if st == state {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// PaperSettings returns a realistic settings payload for the Paper egg.
func PaperSettings(uuid string, port int, memoryMiB int) map[string]any {
	return map[string]any{
		"id": 1, "uuid": uuid,
		"meta":      map[string]any{"name": "Spike", "description": ""},
		"suspended": false,
		"environment": map[string]any{
			"SERVER_JARFILE": "server.jar", "MINECRAFT_VERSION": "latest", "BUILD_NUMBER": "latest",
			"STARTUP":       "java -Xms128M -XX:MaxRAMPercentage=95.0 -Dterminal.jline=false -Dterminal.ansi=true -jar {{SERVER_JARFILE}}",
			"P_SERVER_UUID": uuid, "P_SERVER_ALLOCATION_LIMIT": 1,
		},
		"invocation":       "java -Xms128M -XX:MaxRAMPercentage=95.0 -Dterminal.jline=false -Dterminal.ansi=true -jar {{SERVER_JARFILE}}",
		"skip_egg_scripts": false,
		"build":            map[string]any{"memory_limit": memoryMiB, "swap": 0, "io_weight": 500, "cpu_limit": 0, "threads": nil, "disk_space": 5120, "oom_killer": true},
		"container":        map[string]any{"image": "ghcr.io/pelican-eggs/yolks:java_25", "requires_rebuild": false},
		"allocations":      map[string]any{"force_outgoing_ip": false, "default": map[string]any{"ip": "0.0.0.0", "port": port}, "mappings": map[string][]int{"0.0.0.0": {port}}},
		"egg":              map[string]any{"id": "5c7f5e0b-0000-4000-8000-000000000000", "file_denylist": []string{}, "features": map[string][]string{"eula": {"You need to agree to the EULA"}}},
		"labels":           map[string]any{},
		"mounts":           []any{},
	}
}

// PaperProcessConfiguration returns the Paper egg process configuration.
func PaperProcessConfiguration() map[string]any {
	return map[string]any{
		"startup": map[string]any{"done": []string{")! For help, type "}, "user_interaction": []string{}, "strip_ansi": false},
		"stop":    map[string]any{"type": "command", "value": "stop"},
		"configs": []map[string]any{{"file": "server.properties", "parser": "properties", "replace": []map[string]any{{"match": "server-ip", "replace_with": "0.0.0.0"}, {"match": "server-port", "replace_with": "{{server.build.default.port}}"}, {"match": "query.port", "replace_with": "{{server.build.default.port}}"}}}},
	}
}
