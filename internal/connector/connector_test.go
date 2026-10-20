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
		if strings.HasSuffix(p, "/test") { // "connection" works iff the key is radkey
			var o obj
			json.NewDecoder(r.Body).Decode(&o)
			if o.field("apiKey") != "radkey" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode([]map[string]string{{"propertyName": "ApiKey", "errorMessage": "Unable to connect: 401"}})
				return
			}
			w.Write([]byte("{}"))
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
	target := func() Target {
		t2, skip := prowlarr{}.Target(env, pro, rad)
		if t2 == nil {
			t.Fatal(skip)
		}
		return t2
	}
	plan := func() []Change {
		ch, err := target().Plan(ctx, env.Adopt)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	if found, err := target().Test(ctx); found || err != nil {
		t.Fatalf("test before create: found=%v err=%v", found, err)
	}
	ch := plan()
	if len(ch) != 1 || ch[0].Op != "create" || !strings.Contains(ch[0].Detail, "baseUrl: http://localhost:7878 -> http://radarr_server_1:7878") {
		t.Fatalf("first plan: %+v", ch)
	}
	if err := ch[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	app := store["/applications"][0]
	if app["syncLevel"] != "fullSync" || app.field("prowlarrUrl") != "http://prowlarr_server_1:9696" || app.field("apiKey") != "radkey" || len(app["tags"].([]any)) != 0 || app["name"] != "[arrglue] radarr_server_1" {
		t.Fatalf("created: %+v", app)
	}
	if ch := plan(); len(ch) != 1 || ch[0].Op != "noop" {
		t.Fatalf("second plan should be noop: %+v", ch)
	}
	if found, err := target().Test(ctx); !found || err != nil {
		t.Fatalf("test after create: found=%v err=%v", found, err)
	}
	// API masks secrets on read: must not loop on updates... unless the test fails
	store["/applications"][0].set("apiKey", "********", true)
	if ch := plan(); ch[0].Op != "update" || !strings.Contains(ch[0].Detail, "fails its test") {
		t.Fatalf("masked key failing its test should be re-sent: %+v", ch)
	}
	store["/applications"][0].set("apiKey", "radkey", true)
	if ch := plan(); ch[0].Op != "noop" {
		t.Fatalf("working object should be noop: %+v", ch)
	}
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
	if found, err := target().Test(ctx); !found || err == nil || !strings.Contains(err.Error(), "Unable to connect") {
		t.Fatalf("test with wrong key should fail with the API message: found=%v err=%v", found, err)
	}
	// tagged by an older arrglue -> ours: the tag goes (it filters indexers), the prefix comes
	store["/tag"] = []obj{{"id": 7.0, "label": "arrglue"}, {"id": 8.0, "label": "4k"}}
	store["/applications"][0]["name"] = "radarr_server_1"
	store["/applications"][0]["tags"] = []any{7.0, 8.0}
	ch = plan()
	if ch[0].Op != "update" || ch[0].Apply(ctx) != nil {
		t.Fatalf("legacy tag: %+v", ch)
	}
	if app := store["/applications"][0]; app["name"] != "[arrglue] radarr_server_1" || len(app["tags"].([]any)) != 1 || app["tags"].([]any)[0] != 8.0 {
		t.Fatalf("legacy tag not migrated: %+v", app)
	}
	// hand-made object -> skip, unless adopting
	store["/applications"][0]["name"] = "Radarr"
	if ch := plan(); ch[0].Op != "skip" {
		t.Fatalf("unmanaged should skip: %+v", ch)
	}
	env.Adopt = true
	ch = plan()
	if ch[0].Op != "adopt" || ch[0].Apply(ctx) != nil || store["/applications"][0]["name"] != "[arrglue] Radarr" {
		t.Fatalf("adopt: %+v name=%v", ch, store["/applications"][0]["name"])
	}
}
