package main

import (
	"context"
	"encoding/json"
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
// and plan in memory, expose them and the registry over HTTP.
// ponytail: no SQLite history, no web UI yet; secrets are only served to the
// bearer of ARRGLUE_TOKEN and never written to disk.
func serveCmd() *cobra.Command {
	var listen string
	var interval time.Duration
	var autoApply, adopt bool
	cmd := &cobra.Command{Use: "serve", Short: "Run as a daemon: rescan on Docker events, expose the registry API", RunE: func(cmd *cobra.Command, _ []string) error {
		s := &server{token: os.Getenv("ARRGLUE_TOKEN"), adopt: adopt, autoApply: autoApply}
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
	cmd.Flags().BoolVar(&adopt, "adopt", false, "take over equivalent objects created by hand")
	return cmd
}

type server struct {
	mu               sync.RWMutex
	st               *state
	rep              *Report
	changes          []connector.Change
	at               time.Time
	err              error
	token            string
	adopt, autoApply bool
}

func (s *server) cycle(ctx context.Context) {
	st, err := load(ctx, true)
	if err != nil {
		log.Printf("cycle failed: %v", err)
		s.mu.Lock()
		s.err, s.at = err, time.Now()
		s.mu.Unlock()
		return
	}
	rep := report(ctx, st)
	st.env.Adopt = s.adopt
	changes, err := engine.Plan(ctx, st.env, st.cat, st.insts)
	if err != nil {
		log.Printf("plan failed: %v", err)
	}
	todo, applied := 0, 0
	for _, c := range changes {
		if c.Apply == nil {
			continue
		}
		todo++
		if !s.autoApply {
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
	s.st, s.rep, s.changes, s.err, s.at = st, rep, changes, nil, time.Now()
	s.mu.Unlock()
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
	out := map[string]any{"last_cycle": s.at, "auto_apply": s.autoApply}
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
	type change struct{ Link, Op, What, Detail string }
	out := []change{}
	for _, c := range s.changes {
		out = append(out, change{c.Link, c.Op, c.What, c.Detail})
	}
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
