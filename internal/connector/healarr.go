package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// healarr -> radarr/sonarr: an *arr instance in Healarr. Its API key is
// encrypted at rest, so it has to come from --secrets.
type healarr struct{}

func (healarr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "healarr" && (to.Kind == "radarr" || to.Kind == "sonarr")
}

func (healarr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	if from.Secret.Value == "" {
		return nil, "Healarr api key unknown: copy it from Healarr → Config → API key into --secrets (" + from.Name + ": <key>)"
	}
	a := jsonAPI(env.URL(nil, from), "/api", "X-API-Key", from.Secret.Value)
	return &healarrArr{a: a, link: link, kind: to.Kind, name: to.Name, url: toURL, key: to.Secret.Value}, ""
}

type healarrArr struct {
	a                          api
	link, kind, name, url, key string
}

func (t *healarrArr) What() string { return fmt.Sprintf("%s instance %q", t.kind, t.name) }

func (t *healarrArr) find(ctx context.Context) (obj, error) {
	var st struct {
		IsSetup bool `json:"is_setup"`
	}
	if err := t.a.do(ctx, "GET", "/auth/status", nil, &st); err != nil {
		return nil, err
	}
	if !st.IsSetup {
		return nil, Pending("Healarr first-run setup not done yet (set its password once)")
	}
	var list []obj
	if err := t.a.do(ctx, "GET", "/config/arr", nil, &list); err != nil {
		return nil, err
	}
	for _, o := range list {
		if sameEndpoint(fmt.Sprint(o["url"]), t.url) {
			return o, nil
		}
	}
	return nil, nil
}

func (t *healarrArr) Test(ctx context.Context) (bool, error) {
	o, err := t.find(ctx)
	if err != nil || o == nil {
		return false, err
	}
	// the stored key is not readable: test with the key we would set
	return true, t.a.do(ctx, "POST", "/config/arr/test", obj{"type": t.kind, "url": t.url, "api_key": t.key}, nil)
}

func (t *healarrArr) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	o, err := t.find(ctx)
	if p, ok := err.(Pending); ok {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: string(p)}}, nil
	}
	if err != nil {
		return nil, err
	}
	body := obj{"name": seerrPrefix + t.name, "type": t.kind, "url": t.url, "api_key": t.key, "enabled": true}
	if o == nil {
		return []Change{{Link: t.link, Op: "create", What: t.What(), Detail: "  name: " + seerrPrefix + t.name + "\n  url: " + t.url, Apply: func(ctx context.Context) error {
			return t.a.do(ctx, "POST", "/config/arr", body, nil)
		}}}, nil
	}
	name := fmt.Sprint(o["name"])
	op, d := "update", []string{}
	if !strings.HasPrefix(name, seerrPrefix) {
		if !adopt {
			return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: fmt.Sprintf("%q exists without the %s prefix", name, strings.TrimSpace(seerrPrefix))}}, nil
		}
		op = "adopt"
		body["name"] = seerrPrefix + name
		d = append(d, fmt.Sprintf("  name: %s -> %s", name, body["name"]))
	} else {
		body["name"] = name
	}
	if o["enabled"] != true {
		d = append(d, "  enabled: true")
	}
	// the key is write-only: re-send it when Healarr cannot talk to the app
	if err := t.a.do(ctx, "POST", "/config/arr/test", obj{"type": t.kind, "url": t.url, "api_key": t.key}, nil); err == nil {
		if _, testErr := t.Test(ctx); testErr != nil {
			d = append(d, "  api_key: *** -> *** (re-sent: "+testErr.Error()+")")
		}
	}
	if op == "update" && len(d) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	id := strings.TrimSuffix(fmt.Sprint(o["id"]), ".0")
	return []Change{{Link: t.link, Op: op, What: t.What(), Detail: strings.Join(d, "\n"), Apply: func(ctx context.Context) error {
		return t.a.do(ctx, "PUT", "/config/arr/"+id, body, nil)
	}}}, nil
}
