package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// fakeSeerr: /settings/radarr list/create/update, /settings/radarr/test
// answers with profiles and root folders iff the key is right.
func TestSeerr(t *testing.T) {
	var servers []obj
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "seerrkey" && r.URL.Path != "/api/v1/settings/public" {
			http.Error(w, "forbidden", 403)
			return
		}
		var body obj
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.URL.Path == "/api/v1/settings/public":
			w.Write([]byte(`{"initialized":true}`))
		case r.URL.Path == "/api/v1/settings/radarr/test":
			if body["apiKey"] != "radkey" {
				http.Error(w, `{"message":"Failed to connect"}`, 500)
				return
			}
			json.NewEncoder(w).Encode(obj{"profiles": []any{obj{"id": 4.0, "name": "HD-1080p"}}, "rootFolders": []any{obj{"id": 1.0, "path": "/movies"}}})
		case r.URL.Path == "/api/v1/settings/radarr" && r.Method == "GET":
			json.NewEncoder(w).Encode(append([]obj{}, servers...))
		case r.URL.Path == "/api/v1/settings/radarr" && r.Method == "POST":
			body["id"] = float64(len(servers))
			servers = append(servers, body)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(body)
		case strings.HasPrefix(r.URL.Path, "/api/v1/settings/radarr/") && r.Method == "PUT":
			servers[0] = body
			json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cat, _ := catalog.Load("")
	net := []string{"umbrel_main_network"}
	se := &graph.Instance{ID: "docker:seerr", Kind: "seerr", Name: "jellyseerr_server_1", Networks: net, Secret: graph.SecretRef{Value: "seerrkey"}}
	rad := &graph.Instance{ID: "docker:radarr", Kind: "radarr", Name: "radarr_server_1", Networks: net, Secret: graph.SecretRef{Value: "radkey"}}
	env := &Env{Cat: cat, URL: func(from, to *graph.Instance) string {
		if from == nil {
			return srv.URL
		}
		return "http://radarr_server_1:7878"
	}}
	ctx := context.Background()
	target := func() Target {
		tg, skip := seerr{}.Target(env, se, rad)
		if tg == nil {
			t.Fatal(skip)
		}
		return tg
	}
	ch, err := target().Plan(ctx, false)
	if err != nil || len(ch) != 1 || ch[0].Op != "create" || !strings.Contains(ch[0].Detail, "profile: <nil> -> HD-1080p") || !strings.Contains(ch[0].Detail, "rootFolder: <nil> -> /movies") {
		t.Fatalf("create plan: %+v %v", ch, err)
	}
	if err := ch[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	s := servers[0]
	if s["name"] != "[arrglue] radarr_server_1" || s["hostname"] != "radarr_server_1" || s["port"] != 7878.0 || s["activeProfileId"] != 4.0 || s["activeDirectory"] != "/movies" || s["isDefault"] != true || s["minimumAvailability"] != "released" {
		t.Fatalf("created: %+v", s)
	}
	if ch, _ := target().Plan(ctx, false); ch[0].Op != "noop" {
		t.Fatalf("second plan: %+v", ch)
	}
	if found, err := target().Test(ctx); !found || err != nil {
		t.Fatalf("test: %v %v", found, err)
	}
	// hand-made entry (no prefix): skip, adopt renames and fixes the key
	servers[0]["name"], servers[0]["apiKey"] = "Radarr", "stale"
	if ch, _ := target().Plan(ctx, false); ch[0].Op != "skip" {
		t.Fatalf("unmanaged: %+v", ch)
	}
	if found, err := target().Test(ctx); !found || err == nil {
		t.Fatalf("stale key should fail the test: %v %v", found, err)
	}
	ch, _ = target().Plan(ctx, true)
	if ch[0].Op != "adopt" || ch[0].Apply(ctx) != nil || servers[0]["name"] != "[arrglue] Radarr" || servers[0]["apiKey"] != "radkey" {
		t.Fatalf("adopt: %+v -> %+v", ch, servers[0])
	}
}
