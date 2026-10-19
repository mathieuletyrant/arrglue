package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// profilarr -> radarr/sonarr: Profilarr's public API only lists its *arr
// instances, so arrglue reads its database (url and key per instance) to
// check them and tells what to set in the UI (manual change).
type profilarr struct{}

func (profilarr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "profilarr" && (to.Kind == "radarr" || to.Kind == "sonarr")
}

func (profilarr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	db, ok := from.ToHost("/config/data/profilarr.db")
	if !ok {
		return nil, "Profilarr's /config is not mounted: cannot read its instances"
	}
	return &profilarrEntry{link: link, from: from, kind: to.Kind, name: to.Name, url: toURL, key: to.Secret.Value, db: db}, ""
}

type profilarrEntry struct {
	link                     string
	from                     *graph.Instance
	kind, name, url, key, db string
}

func (t *profilarrEntry) What() string { return fmt.Sprintf("%s instance %q", t.kind, t.name) }

func (t *profilarrEntry) find() (map[string]any, error) {
	rows, err := catalog.QueryDB(t.db, "SELECT name, url, api_key, enabled FROM arr_instances WHERE type = '"+t.kind+"'")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if sameEndpoint(fmt.Sprint(r["url"]), t.url) {
			return r, nil
		}
	}
	return nil, nil
}

func (t *profilarrEntry) Test(ctx context.Context) (bool, error) {
	r, err := t.find()
	if err != nil || r == nil {
		return false, err
	}
	if fmt.Sprint(r["api_key"]) != t.key {
		return true, fmt.Errorf("Profilarr holds a stale key for %s (run plan)", t.name)
	}
	if fmt.Sprint(r["enabled"]) == "0" {
		return true, fmt.Errorf("instance %q is disabled in Profilarr", r["name"])
	}
	return true, nil
}

func (t *profilarrEntry) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	r, err := t.find()
	if err != nil {
		return nil, err
	}
	if r != nil && fmt.Sprint(r["api_key"]) == t.key && fmt.Sprint(r["enabled"]) != "0" {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	what := "add"
	if r != nil {
		what = fmt.Sprintf("edit %q", r["name"])
	}
	return []Change{{Link: t.link, Op: "manual", What: t.What(), Detail: fmt.Sprintf("in Profilarr → Settings → Arr Instances, %s a %s instance:\n  URL: %s\n  API key: %s\n(Profilarr's API cannot write instances)", what, strings.Title(t.kind), t.url, t.key)}}, nil
}
