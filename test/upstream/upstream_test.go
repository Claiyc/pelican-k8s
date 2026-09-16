// Package upstream guards the gateway against Wings changes: it parses the
// pinned Wings module's router and remote client and fails when a route or
// remote-API call appears that the gateway does not handle (ARCHITECTURE.md 18).
package upstream

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wingsDir locates the Wings module in the module cache.
func wingsDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/pelican/wings").Output()
	if err != nil {
		t.Skipf("wings module not available: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Skip("wings module dir unknown")
	}
	return dir
}

type route struct{ method, path string }

// wingsRoutes extracts every route registered in router/router.go.
func wingsRoutes(t *testing.T, dir string) []route {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(dir, "router", "router.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []route
	groups := map[string]string{"router": "", "protected": ""}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			// name := <recv>.Group("/prefix")
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Group" && len(call.Args) > 0 {
						if lit, ok := call.Args[0].(*ast.BasicLit); ok {
							recv := exprName(sel.X)
							name := exprName(x.Lhs[0])
							groups[name] = groups[recv] + strings.Trim(lit.Value, `"`)
						}
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			method := sel.Sel.Name
			switch method {
			case "GET", "POST", "PUT", "DELETE", "PATCH":
			default:
				return true
			}
			if len(x.Args) == 0 {
				return true
			}
			lit, ok := x.Args[0].(*ast.BasicLit)
			if !ok {
				return true
			}
			prefix, known := groups[exprName(sel.X)]
			if !known {
				return true
			}
			p := strings.Trim(lit.Value, `"`)
			if p != "" && !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			routes = append(routes, route{method: method, path: prefix + p})
		}
		return true
	})
	return routes
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// gatewayRoutes lists the routes the gateway handles explicitly; everything
// else under /api/servers/:server/ is proxied by prefix.
var gatewayRoutes = map[route]string{
	{"GET", "/download/backup"}: "signedProxy", {"GET", "/download/file"}: "signedProxy", {"POST", "/upload/file"}: "signedProxy",
	{"GET", "/api/servers/:server/ws"}: "websocket", {"POST", "/api/transfers"}: "unsupported",
	{"POST", "/api/update"}: "update", {"GET", "/api/system"}: "systemInfo", {"GET", "/api/diagnostics"}: "diagnostics",
	{"GET", "/api/system/docker/disk"}: "stub", {"DELETE", "/api/system/docker/image/prune"}: "stub",
	{"GET", "/api/system/ips"}: "systemIPs", {"GET", "/api/system/utilization"}: "utilization",
	{"GET", "/api/servers"}: "listServers", {"POST", "/api/servers"}: "createServer",
	{"DELETE", "/api/transfers/:server"}: "unsupported", {"POST", "/api/deauthorize-user"}: "deauthorize",
	{"GET", "/api/servers/:server"}: "getServer", {"DELETE", "/api/servers/:server"}: "deleteServer",
	{"POST", "/api/servers/:server/sync"}: "syncServer", {"POST", "/api/servers/:server/install"}: "install",
	{"POST", "/api/servers/:server/reinstall"}: "install", {"POST", "/api/servers/:server/power"}: "power",
	{"POST", "/api/servers/:server/transfer"}: "unsupported", {"DELETE", "/api/servers/:server/transfer"}: "unsupported",
	{"POST", "/api/servers/:server/backup"}: "backupProxy", {"POST", "/api/servers/:server/backup/:backup/restore"}: "backupProxy",
}

// proxiedByPrefix are Wings routes the gateway forwards unchanged. Listing them
// makes upstream additions visible: a new route fails this test until it is
// reviewed and added here or handled explicitly.
var proxiedByPrefix = []route{
	{"GET", "/api/servers/:server/logs"}, {"GET", "/api/servers/:server/install-logs"},
	{"POST", "/api/servers/:server/commands"}, {"POST", "/api/servers/:server/ws/deny"},
	{"DELETE", "/api/servers/:server/deleteAllBackups"},
	{"GET", "/api/servers/:server/files/contents"}, {"GET", "/api/servers/:server/files/list-directory"},
	{"PUT", "/api/servers/:server/files/rename"}, {"POST", "/api/servers/:server/files/copy"},
	{"POST", "/api/servers/:server/files/write"}, {"POST", "/api/servers/:server/files/create-directory"},
	{"POST", "/api/servers/:server/files/delete"}, {"POST", "/api/servers/:server/files/compress"},
	{"POST", "/api/servers/:server/files/decompress"}, {"POST", "/api/servers/:server/files/chmod"},
	{"GET", "/api/servers/:server/files/search"}, {"GET", "/api/servers/:server/files/pull"},
	{"POST", "/api/servers/:server/files/pull"}, {"DELETE", "/api/servers/:server/files/pull/:download"},
	{"DELETE", "/api/servers/:server/backup/:backup"},
}

func TestGatewayCoversWingsRoutes(t *testing.T) {
	dir := wingsDir(t)
	routes := wingsRoutes(t, dir)
	if len(routes) < 40 {
		t.Fatalf("parsed only %d routes from wings router.go; parser broken?", len(routes))
	}
	proxied := map[route]bool{}
	for _, r := range proxiedByPrefix {
		proxied[r] = true
	}
	var unknown []string
	for _, r := range routes {
		if _, ok := gatewayRoutes[r]; ok {
			continue
		}
		if proxied[r] {
			continue
		}
		unknown = append(unknown, r.method+" "+r.path)
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Fatalf("Wings routes not covered by the gateway (handle them or add them to proxiedByPrefix after review):\n  %s", strings.Join(unknown, "\n  "))
	}
	// And the reverse: every route we claim exists upstream.
	have := map[route]bool{}
	for _, r := range routes {
		have[r] = true
	}
	var stale []string
	for r := range gatewayRoutes {
		if !have[r] {
			stale = append(stale, r.method+" "+r.path)
		}
	}
	for _, r := range proxiedByPrefix {
		if !have[r] {
			stale = append(stale, r.method+" "+r.path)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("routes listed by the gateway no longer exist in Wings:\n  %s", strings.Join(stale, "\n  "))
	}
}

// remoteCalls extracts the paths Wings' remote client calls on the Panel.
func remoteCalls(t *testing.T, dir string) []string {
	t.Helper()
	re := regexp.MustCompile(`c\.(Get|Post)\(ctx, (?:fmt\.Sprintf\()?"([^"]+)"`)
	var calls []string
	entries, _ := filepath.Glob(filepath.Join(dir, "remote", "*.go"))
	for _, p := range entries {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			calls = append(calls, m[1]+" "+m[2])
		}
	}
	sort.Strings(calls)
	return calls
}

// remoteAPI is what the gateway serves to agents (remoteapi package).
var remoteAPI = map[string]string{
	"Get /servers":                      "listServers",
	"Post /servers/reset":               "ack",
	"Get /servers/%s":                   "getServer",
	"Get /servers/%s/install":           "getInstall",
	"Post /servers/%s/install":          "installResult",
	"Post /servers/%s/archive":          "not served: dead Panel route (contract doc 3)",
	"Post /servers/%s/transfer/%s":      "not served: transfers unsupported",
	"Post /sftp/auth":                   "sftpAuth",
	"Get /backups/%s":                   "backup",
	"Post /backups/%s":                  "backup",
	"Post /backups/%s/restore":          "backup",
	"Post /activity":                    "activity",
	"Post /servers/%s/container/status": "containerStatus",
}

func TestGatewayServesWingsRemoteCalls(t *testing.T) {
	dir := wingsDir(t)
	calls := remoteCalls(t, dir)
	if len(calls) < 10 {
		t.Fatalf("parsed only %d remote calls; parser broken?", len(calls))
	}
	var unknown []string
	for _, c := range calls {
		if _, ok := remoteAPI[c]; !ok {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("Wings remote client calls the gateway does not serve:\n  %s", strings.Join(unknown, "\n  "))
	}
}

// TestProcessEnvironmentInterface fails to compile the agent when Wings adds
// methods to ProcessEnvironment; this test just documents where to look.
func TestProcessEnvironmentInterface(t *testing.T) {
	dir := wingsDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "environment", "environment.go"))
	if err != nil {
		t.Fatal(err)
	}
	methods := regexp.MustCompile(`(?m)^\s+([A-Z][A-Za-z]+)\(`).FindAllStringSubmatch(string(b), -1)
	want := []string{"Type", "Config", "Events", "Exists", "IsRunning", "InSituUpdate", "OnBeforeStart", "Start", "Stop", "WaitForStop", "Terminate", "Destroy", "ExitState", "Create", "Attach", "SendCommand", "Readlog", "State", "SetState", "Uptime", "SetLogCallback"}
	have := map[string]bool{}
	for _, m := range methods {
		have[m[1]] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("ProcessEnvironment method %s missing upstream", w)
		}
	}
	for m := range have {
		found := false
		for _, w := range want {
			if w == m {
				found = true
			}
		}
		if !found {
			t.Errorf("new ProcessEnvironment method upstream: %s (implement it in internal/agent/shimenv)", m)
		}
	}
}
