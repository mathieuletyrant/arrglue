package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathieuletyrant/argue/internal/catalog"
)

// Replays testdata/containers.json (an Umbrel stack: <app>_server_1 on
// umbrel_main_network, ports published by app_proxy only) with app-data
// rewritten to testdata/app-data so real config files get parsed.
func TestDiscover(t *testing.T) {
	raw, _ := os.ReadFile("testdata/containers.json")
	appData, _ := filepath.Abs("testdata/app-data")
	body := strings.ReplaceAll(string(raw), "/home/umbrel/umbrel/app-data", appData)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	cat, _ := catalog.Load("")
	got, err := (&Client{HTTP: srv.Client(), Base: srv.URL}).Discover(context.Background(), cat)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ // id -> kind
		"docker:radarr_server_1":       "radarr",
		"docker:sonarr_server_1":       "sonarr",
		"docker:prowlarr_server_1":     "prowlarr",
		"docker:transmission_server_1": "transmission",
		"docker:jellyfin_server_1":     "jellyfin",
		"docker:jellyseerr_server_1":   "seerr",
		"docker:custom_arr_1":          "sonarr", // via label arrglue.kind
	}
	if len(got) != len(want) {
		t.Fatalf("got %d instances, want %d: %+v", len(got), len(want), got)
	}
	by := map[string]int{}
	for i, in := range got {
		by[in.ID] = i
		if want[in.ID] != in.Kind {
			t.Errorf("%s: kind %q want %q", in.ID, in.Kind, want[in.ID])
		}
		if len(in.Networks) == 0 || in.Networks[0] != "umbrel_main_network" {
			t.Errorf("%s: networks %v", in.ID, in.Networks)
		}
	}
	radarr := got[by["docker:radarr_server_1"]]
	if radarr.Secret.Value != "0123456789abcdef0123456789abcdef" || radarr.Config["port"] != "7878" || radarr.PublishedPort(7878) != 0 {
		t.Errorf("radarr: secret=%q config=%v ports=%v errs=%v", radarr.Secret.Value, radarr.Config, radarr.Ports, radarr.Errors)
	}
	if h, ok := radarr.ToHost("/movies/Dune (2021)"); !ok || h != "/home/umbrel/umbrel/data/storage/movies/Dune (2021)" {
		t.Errorf("ToHost: %q %v", h, ok)
	}
	seerr := got[by["docker:jellyseerr_server_1"]]
	if seerr.Secret.Value != "seerr-api-key-xyz" {
		t.Errorf("seerr (config under /app/config): secret=%q errs=%v", seerr.Secret.Value, seerr.Errors)
	}
	if got[by["docker:jellyfin_server_1"]].Config["port"] != "8096" {
		t.Errorf("jellyfin: %+v", got[by["docker:jellyfin_server_1"]])
	}
	if got[by["docker:transmission_server_1"]].Config["auth_required"] != "false" {
		t.Errorf("transmission: %+v", got[by["docker:transmission_server_1"]])
	}
	if len(got[by["docker:custom_arr_1"]].Errors) == 0 {
		t.Errorf("custom_arr_1 has no mounts: expected a config error")
	}
}
