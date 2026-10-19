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

// fakeServarr stores objects per collection, serves /schema and /tag.
func fakeServarr(t *testing.T, schemas map[string][]obj) (*httptest.Server, map[string][]obj) {
	store := map[string][]obj{}
	next := 1.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		p = strings.TrimPrefix(p, "/api/v3")
		if strings.HasSuffix(p, "/schema") {
			json.NewEncoder(w).Encode(schemas[strings.TrimSuffix(p, "/schema")])
			return
		}
		coll, id, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(append([]obj{}, store["/"+coll]...))
		case "POST":
			var o obj
			json.NewDecoder(r.Body).Decode(&o)
			o["id"] = next
			next++
			store["/"+coll] = append(store["/"+coll], o)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(o)
		case "PUT":
			var o obj
			json.NewDecoder(r.Body).Decode(&o)
			for i, e := range store["/"+coll] {
				if e["id"].(float64) == o["id"].(float64) && id != "" {
					store["/"+coll][i] = o
				}
			}
			json.NewEncoder(w).Encode(o)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, store
}

func TestProwlarr(t *testing.T) {
	cat, _ := catalog.Load("")
	srv, store := fakeServarr(t, map[string][]obj{"/applications": {{
		"implementation": "Radarr", "configContract": "RadarrSettings", "syncLevel": "addOnly",
		"fields": []any{
			map[string]any{"name": "prowlarrUrl", "value": "http://localhost:9696"},
			map[string]any{"name": "baseUrl", "value": "http://localhost:7878"},
			map[string]any{"name": "apiKey", "value": ""},
			map[string]any{"name": "syncCategories", "value": []any{2000.0}},
		}}}})
	net := []string{"umbrel_main_network"}
	pro := &graph.Instance{ID: "docker:prowlarr_server_1", Kind: "prowlarr", Name: "prowlarr_server_1", Networks: net, Secret: graph.SecretRef{Value: "key"}}
	rad := &graph.Instance{ID: "docker:radarr_server_1", Kind: "radarr", Name: "radarr_server_1", Networks: net, Secret: graph.SecretRef{Value: "radkey"}}
	env := &Env{Cat: cat, URL: func(from, to *graph.Instance) string {
		if from == nil {
			return srv.URL
		}
		return "http://" + to.Name + ":" + map[string]string{"radarr": "7878", "prowlarr": "9696"}[to.Kind]
	}}
	ctx := context.Background()
	plan := func() []Change {
		ch, err := prowlarr{}.Plan(ctx, env, pro, rad)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	ch := plan()
	if len(ch) != 1 || ch[0].Op != "create" || !strings.Contains(ch[0].Detail, "baseUrl: http://localhost:7878 -> http://radarr_server_1:7878") {
		t.Fatalf("first plan: %+v", ch)
	}
	if err := ch[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	app := store["/applications"][0]
	if app["syncLevel"] != "fullSync" || app.field("prowlarrUrl") != "http://prowlarr_server_1:9696" || app.field("apiKey") != "radkey" || !app.hasTag(1) || app["name"] != "radarr_server_1" {
		t.Fatalf("created: %+v", app)
	}
	if ch := plan(); len(ch) != 1 || ch[0].Op != "noop" {
		t.Fatalf("second plan should be noop: %+v", ch)
	}
	// API masks secrets on read: must not loop on updates
	store["/applications"][0].set("apiKey", "********", true)
	if ch := plan(); ch[0].Op != "noop" {
		t.Fatalf("masked key should be noop: %+v", ch)
	}
	store["/applications"][0].set("apiKey", "radkey", true)
	// key rotated -> update
	rad.Secret.Value = "newkey"
	ch = plan()
	if ch[0].Op != "update" || !strings.Contains(ch[0].Detail, "apiKey: *** -> ***") {
		t.Fatalf("after key change: %+v", ch)
	}
	if err := ch[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if store["/applications"][0].field("apiKey") != "newkey" {
		t.Fatal("update not persisted")
	}
	// hand-made object without tag -> skip, unless adopting
	store["/applications"][0]["tags"] = []any{}
	if ch := plan(); ch[0].Op != "skip" {
		t.Fatalf("unmanaged should skip: %+v", ch)
	}
	env.Adopt = true
	ch = plan()
	if ch[0].Op != "adopt" || ch[0].Apply(ctx) != nil || !store["/applications"][0].hasTag(1) {
		t.Fatalf("adopt: %+v tags=%v", ch, store["/applications"][0]["tags"])
	}
}
