package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalog(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for img, kind := range map[string]string{
		"lscr.io/linuxserver/radarr:latest": "radarr",
		"hotio/sonarr@sha256:abc":           "sonarr",
		"fallenbagel/jellyseerr:2.1.0":      "seerr",
		"linuxserver/jellyfin":              "jellyfin",
		"nginx:1.25":                        "",
	} {
		a := c.MatchImage(img)
		got := ""
		if a != nil {
			got = a.Kind
		}
		if got != kind {
			t.Errorf("%s: got %q want %q", img, got, kind)
		}
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	write("config.xml", "<Config><ApiKey>k1</ApiKey><Port>7878</Port><UrlBase></UrlBase></Config>")
	write("settings.json", `{"rpc-port": 9091, "rpc-url": "/transmission/", "rpc-authentication-required": false}`)
	write("seerr.json", `{"main": {"apiKey": "sk"}}`)
	write("network.xml", `<?xml version="1.0"?><NetworkConfiguration><BaseUrl /><InternalHttpPort>8096</InternalHttpPort></NetworkConfiguration>`)

	for kind, want := range map[string]map[string]string{
		"radarr":       {"api_key": "k1", "port": "7878", "url_base": ""},
		"transmission": {"port": "9091", "url_base": "/transmission/", "auth_required": "false"},
		"seerr":        {"api_key": "sk"},
		"jellyfin":     {"port": "8096", "url_base": ""},
	} {
		a := c.Apps[kind]
		file := map[string]string{"radarr": "config.xml", "transmission": "settings.json", "seerr": "seerr.json", "jellyfin": "network.xml"}[kind]
		got, err := a.ReadConfig(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s.%s: got %q want %q", kind, k, got[k], v)
			}
		}
	}
}
