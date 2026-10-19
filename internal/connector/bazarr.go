package connector

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// bazarr -> sonarr/radarr: Bazarr holds one Sonarr and one Radarr (slots),
// saved through its form-encoded settings endpoint. Its status endpoint
// reports the version it gets from each app: empty means not reachable.
type bazarr struct{}

func (bazarr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "bazarr" && (to.Kind == "sonarr" || to.Kind == "radarr")
}

func (bazarr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	if from.Secret.Value == "" {
		return nil, "no api key known for " + from.Name
	}
	host, port, base := hostPort(toURL)
	if base == "" {
		base = "/"
	}
	a := jsonAPI(env.URL(nil, from), "/api/system", "X-API-KEY", from.Secret.Value)
	return &bazarrSlot{a: a, link: link, kind: to.Kind, name: to.Name,
		want: map[string]any{"ip": host, "port": port, "base_url": base, "ssl": false, "apikey": to.Secret.Value}}, ""
}

type bazarrSlot struct {
	a                api
	link, kind, name string
	want             map[string]any
}

func (t *bazarrSlot) What() string { return fmt.Sprintf("%s connection %q", t.kind, t.name) }

// current returns Bazarr's section for the app, empty when the app is not enabled.
func (t *bazarrSlot) current(ctx context.Context) (obj, error) {
	var all map[string]obj
	if err := t.a.do(ctx, "GET", "/settings", nil, &all); err != nil {
		return nil, err
	}
	if all["general"]["use_"+t.kind] != true {
		return obj{}, nil
	}
	return all[t.kind], nil
}

func (t *bazarrSlot) Test(ctx context.Context) (bool, error) {
	cur, err := t.current(ctx)
	if err != nil {
		return false, err
	}
	if len(cur) == 0 {
		return false, nil
	}
	var st struct{ Data map[string]any }
	if err := t.a.do(ctx, "GET", "/status", nil, &st); err != nil {
		return true, err
	}
	if v, _ := st.Data[t.kind+"_version"].(string); v == "" {
		return true, fmt.Errorf("Bazarr cannot reach %s with its current settings", t.name)
	}
	return true, nil
}

func (t *bazarrSlot) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	cur, err := t.current(ctx)
	if err != nil {
		return nil, err
	}
	return []Change{slot(t.link, t.What(), cur, t.want, adopt, func(ctx context.Context) error {
		v := url.Values{"settings-general-use_" + t.kind: {"true"}}
		for k, val := range t.want {
			v.Set("settings-"+t.kind+"-"+k, strings.ToLower(fmt.Sprint(val)))
		}
		return t.a.form(ctx, "/settings", v)
	})}, nil
}
