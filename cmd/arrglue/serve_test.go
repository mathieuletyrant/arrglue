package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathieuletyrant/arrglue/internal/connector"
)

// fakeServer plans the given changes; Apply records what was written.
func fakeServer(t *testing.T, changes func() []connector.Change) (*httptest.Server, *server) {
	s := newServer("", false, false)
	s.scanFn = func(context.Context) (*state, *Report, []connector.Change, error) {
		return &state{}, &Report{Self: "arrglue_server_1 on umbrel_main_network"}, changes(), nil
	}
	s.planFn = func(context.Context) ([]connector.Change, error) { return changes(), nil }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /v1/state", s.snapshot)
	mux.HandleFunc("POST /v1/scan", s.scanNow)
	mux.HandleFunc("POST /v1/apply", s.apply)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url, body string, header map[string]string) *http.Response {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestWebApply(t *testing.T) {
	var written []string
	mk := func(op, what string) connector.Change {
		c := connector.Change{Link: "prowlarr_server_1 -> radarr_server_1", Op: op, What: what}
		if op != "noop" && op != "skip" {
			c.Apply = func(context.Context) error { written = append(written, what); return nil }
		}
		return c
	}
	plan := []connector.Change{mk("create", `application "radarr_server_1"`), mk("adopt", `application "sonarr_server_1"`), mk("noop", "same")}
	srv, s := fakeServer(t, func() []connector.Change { return plan })
	s.cycle(context.Background())

	res, _ := http.Get(srv.URL + "/v1/state")
	var st struct {
		Changes []change `json:"changes"`
	}
	json.NewDecoder(res.Body).Decode(&st)
	if len(st.Changes) != 3 || st.Changes[0].Link != "prowlarr_server_1 → radarr_server_1" || st.Changes[0].Key != `prowlarr_server_1 -> radarr_server_1 | application "radarr_server_1"` {
		t.Fatalf("state: %+v", st.Changes)
	}

	// only the picked change is written; one gone from the fresh plan is reported
	body := `{"keys": [` + strings.Join([]string{jsonString(st.Changes[0].Key), jsonString("gone | x")}, ",") + `]}`
	res = post(t, srv.URL+"/v1/apply", body, nil)
	var out []applied
	json.NewDecoder(res.Body).Decode(&out)
	if len(written) != 1 || written[0] != `application "radarr_server_1"` || !out[0].OK || out[1].OK || !strings.Contains(out[1].Error, "no longer in the plan") {
		t.Fatalf("apply: written=%v out=%+v", written, out)
	}

	// a cross-site form cannot trigger a write
	if res := post(t, srv.URL+"/v1/apply", body, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != 403 {
		t.Fatalf("cross-origin apply: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/scan", strings.NewReader("keys=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("form post: %d", res.StatusCode)
	}
	if res := post(t, srv.URL+"/v1/scan", "{}", map[string]string{"Origin": srv.URL}); res.StatusCode != 202 {
		t.Fatalf("same-origin scan: %d", res.StatusCode)
	}

	res, _ = http.Get(srv.URL + "/")
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("index: %s", ct)
	}
}

// auto-apply never adopts unless serve runs with --adopt.
func TestAutoApplySkipsAdopt(t *testing.T) {
	var written []string
	plan := []connector.Change{
		{Link: "a -> b", Op: "create", What: "x", Apply: func(context.Context) error { written = append(written, "x"); return nil }},
		{Link: "a -> c", Op: "adopt", What: "y", Apply: func(context.Context) error { written = append(written, "y"); return nil }},
	}
	_, s := fakeServer(t, func() []connector.Change { return plan })
	s.autoApply = true
	s.cycle(context.Background())
	if strings.Join(written, ",") != "x" {
		t.Fatalf("auto-apply wrote %v", written)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
