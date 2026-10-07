// Command fakepanel serves the fake Panel (package fakepanel) over HTTP, for
// the live placement suite (test/placement). The suite registers its servers
// with PUT /_fake/servers/{uuid}, a fakepanel.Server as JSON.
//
//	FAKEPANEL_TOKEN_ID=id FAKEPANEL_TOKEN=secret fakepanel
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

func main() {
	addr := os.Getenv("FAKEPANEL_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	p := fakepanel.New(os.Getenv("FAKEPANEL_TOKEN_ID"), os.Getenv("FAKEPANEL_TOKEN"))
	mux := http.NewServeMux()
	mux.Handle("/api/remote/", p.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("PUT /_fake/servers/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		var s fakepanel.Server
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.UUID = r.PathValue("uuid")
		p.Add(&s)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	log.Printf("fake panel listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
