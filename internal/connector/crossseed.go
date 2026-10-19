package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// cross-seed is configured by a JS file arrglue must not write: each link is
// a "manual" change telling what config.js should contain, and the test
// checks whether it does (the provider extracted the arrays as text).
type crossSeed struct{}

func (crossSeed) Matches(from, to *graph.Instance) bool {
	return from.Kind == "cross-seed" && (to.Kind == "prowlarr" || to.Kind == "radarr" || to.Kind == "sonarr" || to.Kind == "transmission")
}

func (crossSeed) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	t := &crossSeedEntry{link: link, from: from, to: to}
	switch to.Kind {
	case "transmission":
		if to.Config["auth_required"] == "true" {
			return nil, to.Name + " requires RPC credentials and arrglue has none"
		}
		t.field, t.key = "torrent_clients", "torrentClients"
		t.entries = []string{fmt.Sprintf("%q", "transmission:"+toURL+"/rpc")}
	case "prowlarr":
		if to.Secret.Value == "" {
			return nil, "no api key known for " + to.Name
		}
		t.field, t.key = "torznab", "torznab"
		var idx []obj
		if err := newAPI(env.URL(nil, to), env.Cat.Apps[to.Kind], to.Secret.Value).do(context.Background(), "GET", "/indexer", nil, &idx); err != nil {
			return nil, "cannot list Prowlarr indexers: " + err.Error()
		}
		for _, i := range idx {
			if i["enable"] == true {
				t.entries = append(t.entries, fmt.Sprintf("%q", fmt.Sprintf("%s/%v/api?apikey=%s", toURL, strings.TrimSuffix(fmt.Sprint(i["id"]), ".0"), to.Secret.Value)))
			}
		}
		if len(t.entries) == 0 {
			return nil, "Prowlarr has no enabled indexer yet"
		}
	default: // radarr, sonarr
		if to.Secret.Value == "" {
			return nil, "no api key known for " + to.Name
		}
		t.field, t.key = to.Kind, to.Kind
		t.entries = []string{fmt.Sprintf("%q", toURL+"/?apikey="+to.Secret.Value)}
	}
	return t, ""
}

type crossSeedEntry struct {
	link       string
	from, to   *graph.Instance
	field, key string   // config field read by the provider, JS key in config.js
	entries    []string // JS string literals that must appear in that array
}

func (t *crossSeedEntry) What() string { return "config.js " + t.key }

func (t *crossSeedEntry) missing() []string {
	have := t.from.Config[t.field]
	var m []string
	for _, e := range t.entries {
		if !strings.Contains(have, strings.Trim(e, `"`)) {
			m = append(m, e)
		}
	}
	return m
}

func (t *crossSeedEntry) Test(ctx context.Context) (bool, error) {
	if t.from.ConfigFile == "" {
		return false, fmt.Errorf("config.js not readable from here")
	}
	if m := t.missing(); len(m) > 0 {
		return true, fmt.Errorf("config.js %s lacks %d entr%s (run plan)", t.key, len(m), map[bool]string{true: "y", false: "ies"}[len(m) == 1])
	}
	return true, nil
}

func (t *crossSeedEntry) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	m := t.missing()
	if len(m) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	file := t.from.ConfigFile
	if file == "" {
		file, _ = t.from.ToHost("/config/config.js")
	}
	return []Change{{Link: t.link, Op: "manual", What: t.What(), Detail: fmt.Sprintf("in %s, add to %s: [\n  %s\n], then restart %s", file, t.key, strings.Join(m, ",\n  "), t.from.Name)}}, nil
}
