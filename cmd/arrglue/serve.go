package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathieuletyrant/arrglue/internal/connector"
	"github.com/mathieuletyrant/arrglue/internal/engine"
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/provider/docker"
	"github.com/mathieuletyrant/arrglue/internal/reach"
)

// serve: rescan on Docker events and on a timer, keep the last doctor report
// and plan in memory, expose them, the registry and the web UI over HTTP.
// ponytail: no SQLite history; secrets are only served to the bearer of
// ARRGLUE_TOKEN and never written to disk.
func serveCmd() *cobra.Command {
	var listen string
	var interval time.Duration
	var autoApply, adopt bool
	cmd := &cobra.Command{Use: "serve", Short: "Run as a daemon: rescan on Docker events, expose the registry API", RunE: func(cmd *cobra.Command, _ []string) error {
		s := newServer(os.Getenv("ARRGLUE_TOKEN"), adopt, autoApply)
		if s.token == "" {
			log.Printf("ARRGLUE_TOKEN not set: the registry will never return api keys")
		}
		go s.loop(cmd.Context(), interval)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /{$}", s.index)
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
		mux.HandleFunc("GET /v1/status", s.status)
		mux.HandleFunc("GET /v1/services", s.services)
		mux.HandleFunc("GET /v1/services/{id}", s.service)
		mux.HandleFunc("GET /v1/doctor", s.doctor)
		mux.HandleFunc("GET /v1/plan", s.plan)
		mux.HandleFunc("GET /v1/state", s.snapshot)
		mux.HandleFunc("POST /v1/scan", s.scanNow)
		mux.HandleFunc("POST /v1/apply", s.apply)
		srv := &http.Server{Addr: listen, Handler: mux}
		go func() {
			<-cmd.Context().Done()
			srv.Shutdown(context.Background())
		}()
		log.Printf("listening on %s", listen)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	}}
	cmd.Flags().StringVar(&listen, "listen", ":8787", "address of the registry API")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Minute, "rescan period, besides Docker events")
	cmd.Flags().BoolVar(&autoApply, "auto-apply", false, "apply the plan at the end of every cycle")
	cmd.Flags().BoolVar(&adopt, "adopt", false, "with --auto-apply, also take over objects created by hand (the web UI always offers them one by one)")
	return cmd
}

type server struct {
	mu               sync.RWMutex
	st               *state
	rep              *Report
	changes          []connector.Change
	at               time.Time
	err              error
	scanning         bool
	token            string
	adopt, autoApply bool

	work   sync.Mutex    // one scan or apply at a time
	rescan chan struct{} // asks the loop for a cycle now
	// scan reads everything; plan only computes the changes. Both plan with
	// adopt on, so hand-made objects show as "adopt", to be picked one by one.
	scanFn func(ctx context.Context) (*state, *Report, []connector.Change, error)
	planFn func(ctx context.Context) ([]connector.Change, error)
}

func newServer(token string, adopt, autoApply bool) *server {
	return &server{token: token, adopt: adopt, autoApply: autoApply, rescan: make(chan struct{}, 1), scanFn: scanAll, planFn: planOnly}
}

func scanAll(ctx context.Context) (*state, *Report, []connector.Change, error) {
	st, err := load(ctx, true)
	if err != nil {
		return nil, nil, nil, err
	}
	rep := report(ctx, st)
	st.env.Adopt = true
	changes, err := engine.Plan(ctx, st.env, st.cat, st.insts)
	if err != nil {
		err = fmt.Errorf("plan: %w", err)
	}
	return st, rep, changes, err
}

func planOnly(ctx context.Context) ([]connector.Change, error) {
	st, err := load(ctx, true)
	if err != nil {
		return nil, err
	}
	st.env.Adopt = true
	return engine.Plan(ctx, st.env, st.cat, st.insts)
}

// key names a change across plans, so the UI can pick one and apply it later.
func key(c connector.Change) string { return c.Link + " | " + c.What }

func (s *server) cycle(ctx context.Context) {
	s.work.Lock()
	defer s.work.Unlock()
	s.mu.Lock()
	s.scanning = true
	s.mu.Unlock()
	st, rep, changes, err := s.scanFn(ctx)
	if st == nil {
		log.Printf("cycle failed: %v", err)
		s.mu.Lock()
		s.err, s.at, s.scanning = err, time.Now(), false
		s.mu.Unlock()
		return
	}
	if err != nil {
		log.Printf("cycle: %v", err)
	}
	todo, applied := 0, 0
	for _, c := range changes {
		if c.Apply == nil {
			continue
		}
		todo++
		if !s.autoApply || (c.Op == "adopt" && !s.adopt) {
			continue
		}
		if err := c.Apply(ctx); err != nil {
			log.Printf("apply %s %s: %v", c.Link, c.What, err)
			continue
		}
		applied++
		log.Printf("applied %s %s", c.Link, c.What)
	}
	log.Printf("cycle: %d apps, %d errors, %d warnings, %d changes pending, %d applied", len(st.insts), rep.Errors, rep.Warnings, todo-applied, applied)
	s.mu.Lock()
	s.st, s.rep, s.changes, s.err, s.at, s.scanning = st, rep, changes, err, time.Now(), false
	s.mu.Unlock()
}

func (s *server) trigger() {
	select {
	case s.rescan <- struct{}{}:
	default: // one is already queued
	}
}

func (s *server) loop(ctx context.Context, interval time.Duration) {
	s.cycle(ctx)
	events, err := docker.New().Events(ctx)
	if err != nil {
		log.Printf("no docker events, timer only: %v", err)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.cycle(ctx)
		case <-s.rescan:
			s.cycle(ctx)
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			switch ev {
			case "start", "die", "destroy", "connect", "disconnect":
				debounce = time.After(3 * time.Second) // containers settle, then one rescan
			}
		case <-debounce:
			debounce = nil
			s.cycle(ctx)
		}
	}
}

func (s *server) write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{"last_cycle": s.at, "auto_apply": s.autoApply, "scanning": s.scanning}
	if s.err != nil {
		out["error"] = s.err.Error()
	}
	if s.rep != nil {
		out["errors"], out["warnings"] = s.rep.Errors, s.rep.Warnings
	}
	s.write(w, out)
}

func (s *server) doctor(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.write(w, s.rep)
}

func (s *server) plan(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.write(w, s.planJSON())
}

type change struct {
	Key    string `json:"key"`
	Link   string `json:"link"`
	Op     string `json:"op"`
	What   string `json:"what"`
	Detail string `json:"detail"`
}

func (s *server) planJSON() []change {
	out := []change{}
	for _, c := range s.changes {
		out = append(out, change{key(c), arrow(c.Link), c.Op, c.What, arrow(c.Detail)})
	}
	return out
}

// state is everything the web UI shows, in one call.
func (s *server) snapshot(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{"scanning": s.scanning, "auto_apply": s.autoApply, "report": s.rep, "changes": s.planJSON()}
	if !s.at.IsZero() {
		out["last_cycle"] = s.at
	}
	if s.err != nil {
		out["error"] = s.err.Error()
	}
	s.write(w, out)
}

// fromPage refuses cross-site posts: a JSON body cannot be sent cross-origin
// without a CORS preflight, which arrglue never answers.
func fromPage(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	o := r.Header.Get("Origin")
	return o == "" || strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://") == r.Host
}

func (s *server) scanNow(w http.ResponseWriter, r *http.Request) {
	if !fromPage(r) {
		http.Error(w, "send JSON from the same origin", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	s.scanning = true // shown at once, before the loop picks it up
	s.mu.Unlock()
	s.trigger()
	w.WriteHeader(http.StatusAccepted)
	s.write(w, map[string]bool{"scanning": true})
}

type applied struct {
	Key   string `json:"key"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// apply writes the chosen changes. It plans again first: what the page shows
// may be minutes old, and only a change still in the fresh plan is applied.
func (s *server) apply(w http.ResponseWriter, r *http.Request) {
	if !fromPage(r) {
		http.Error(w, "send JSON from the same origin", http.StatusForbidden)
		return
	}
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Keys) == 0 {
		http.Error(w, `expected {"keys": ["..."]}`, http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context()) // a closed tab must not cut a write in half
	s.work.Lock()
	changes, err := s.planFn(ctx)
	if err != nil {
		s.work.Unlock()
		http.Error(w, "plan: "+err.Error(), http.StatusBadGateway)
		return
	}
	byKey := map[string]connector.Change{}
	for _, c := range changes {
		if c.Apply != nil {
			byKey[key(c)] = c
		}
	}
	out := []applied{}
	for _, k := range req.Keys {
		c, ok := byKey[k]
		if !ok {
			out = append(out, applied{Key: k, Error: "no longer in the plan: already applied, or the apps changed since the last scan"})
			continue
		}
		if err := c.Apply(ctx); err != nil {
			log.Printf("apply %s %s: %v", c.Link, c.What, err)
			out = append(out, applied{Key: k, Error: err.Error()})
			continue
		}
		log.Printf("applied %s %s (from the web UI)", c.Link, c.What)
		out = append(out, applied{Key: k, OK: true})
	}
	s.work.Unlock()
	s.mu.Lock()
	s.scanning = true
	s.mu.Unlock()
	s.trigger()
	s.write(w, out)
}

type service struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Version  string   `json:"version,omitempty"`
	Networks []string `json:"networks"`
	URL      string   `json:"url,omitempty"`
	Verified bool     `json:"verified"`
	Error    string   `json:"error,omitempty"`
	APIKey   string   `json:"api_key,omitempty"`
}

func (s *server) services(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []service{}
	if s.st != nil {
		for _, in := range s.st.insts {
			out = append(out, service{ID: in.ID, Kind: in.Kind, Name: in.Name, Version: in.Version, Networks: in.Networks, URL: s.st.urls[in.ID], Verified: s.st.urls[in.ID] != ""})
		}
	}
	s.write(w, out)
}

// service answers "how does <for> reach <id>?" and, for the token holder, with what key.
func (s *server) service(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.st == nil {
		http.Error(w, "not scanned yet", 503)
		return
	}
	to := s.find(r.PathValue("id"))
	if to == nil {
		http.NotFound(w, r)
		return
	}
	out := service{ID: to.ID, Kind: to.Kind, Name: to.Name, Version: to.Version, Networks: to.Networks, URL: s.st.urls[to.ID], Verified: s.st.urls[to.ID] != ""}
	if caller := r.URL.Query().Get("for"); caller != "" {
		from := s.find(caller)
		if from == nil {
			http.Error(w, "unknown caller "+caller, 404)
			return
		}
		res := reach.Check(r.Context(), s.st.self, from, to, s.st.cat.Apps[to.Kind])
		out.URL, out.Verified, out.Error = res.URL, res.Verified, res.Err
	}
	if s.token != "" && r.Header.Get("Authorization") == "Bearer "+s.token {
		out.APIKey = to.Secret.Value
	}
	s.write(w, out)
}

func (s *server) find(idOrName string) *graph.Instance {
	for i := range s.st.insts {
		in := &s.st.insts[i]
		if in.ID == idOrName || in.Name == idOrName || strings.TrimPrefix(in.ID, "docker:") == idOrName {
			return in
		}
	}
	return nil
}
