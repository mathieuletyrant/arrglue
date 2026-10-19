package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// cleanuparr -> radarr/sonarr (instances) and transmission (download client).
// The API key belongs to the account created at first run (users.db).
type cleanuparr struct{}

func (cleanuparr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "cleanuparr" && (to.Kind == "radarr" || to.Kind == "sonarr" || to.Kind == "transmission")
}

func (cleanuparr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if from.Secret.Value == "" {
		return nil, "Cleanuparr first-run setup not done yet (create its account once), arrglue then reads its api key"
	}
	a := jsonAPI(env.URL(nil, from), "/api/configuration", "X-Api-Key", from.Secret.Value)
	if to.Kind == "transmission" {
		if to.Config["auth_required"] == "true" {
			return nil, to.Name + " requires RPC credentials and arrglue has none"
		}
		host, port, base := hostPort(toURL)
		return &cleanuparrClient{a: a, link: link, name: to.Name, host: fmt.Sprintf("http://%s:%d", host, port), base: base}, ""
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	return &cleanuparrArr{a: a, link: link, kind: to.Kind, name: to.Name, url: toURL, key: to.Secret.Value}, ""
}

type cleanuparrArr struct {
	a                          api
	link, kind, name, url, key string
}

func (t *cleanuparrArr) What() string { return fmt.Sprintf("%s instance %q", t.kind, t.name) }

func (t *cleanuparrArr) find(ctx context.Context) (obj, error) {
	var cfg struct {
		Instances []obj `json:"instances"`
	}
	if err := t.a.do(ctx, "GET", "/"+t.kind, nil, &cfg); err != nil {
		return nil, err
	}
	for _, o := range cfg.Instances {
		if sameEndpoint(fmt.Sprint(o["url"]), t.url) {
			return o, nil
		}
	}
	return nil, nil
}

func (t *cleanuparrArr) Test(ctx context.Context) (bool, error) {
	o, err := t.find(ctx)
	if err != nil || o == nil {
		return false, err
	}
	return true, t.a.do(ctx, "POST", "/"+t.kind+"/instances/test", obj{"url": o["url"], "apiKey": o["apiKey"], "version": o["version"], "instanceId": o["id"]}, nil)
}

func (t *cleanuparrArr) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	o, err := t.find(ctx)
	if err != nil {
		return nil, err
	}
	body := obj{"enabled": true, "name": seerrPrefix + t.name, "url": t.url, "apiKey": t.key, "version": 3}
	if o == nil {
		return []Change{{Link: t.link, Op: "create", What: t.What(), Detail: "  name: " + seerrPrefix + t.name + "\n  url: " + t.url, Apply: func(ctx context.Context) error {
			return t.a.do(ctx, "POST", "/"+t.kind+"/instances", body, nil)
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
	if o["version"] != nil {
		body["version"] = o["version"]
	}
	if o["enabled"] != true {
		d = append(d, "  enabled: true")
	}
	if fmt.Sprint(o["apiKey"]) != t.key && !masked(o["apiKey"]) {
		d = append(d, "  apiKey: *** -> ***")
	}
	if op == "update" && len(d) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	id := fmt.Sprint(o["id"])
	return []Change{{Link: t.link, Op: op, What: t.What(), Detail: strings.Join(d, "\n"), Apply: func(ctx context.Context) error {
		return t.a.do(ctx, "PUT", "/"+t.kind+"/instances/"+id, body, nil)
	}}}, nil
}

type cleanuparrClient struct {
	a                      api
	link, name, host, base string
}

func (t *cleanuparrClient) What() string { return fmt.Sprintf("download client %q", t.name) }

func (t *cleanuparrClient) find(ctx context.Context) (obj, error) {
	var res struct {
		Clients []obj `json:"clients"`
	}
	if err := t.a.do(ctx, "GET", "/download_client", nil, &res); err != nil {
		return nil, err
	}
	for _, o := range res.Clients {
		if sameEndpoint(fmt.Sprint(o["host"]), t.host) {
			return o, nil
		}
	}
	return nil, nil
}

func (t *cleanuparrClient) body(name string) obj {
	return obj{"enabled": true, "name": name, "typeName": "Transmission", "host": t.host, "urlBase": t.base, "username": "", "password": ""}
}

func (t *cleanuparrClient) Test(ctx context.Context) (bool, error) {
	o, err := t.find(ctx)
	if err != nil || o == nil {
		return false, err
	}
	return true, t.a.do(ctx, "POST", "/download_client/test", obj{"typeName": o["typeName"], "host": o["host"], "urlBase": o["urlBase"], "username": o["username"], "password": o["password"], "clientId": o["id"]}, nil)
}

func (t *cleanuparrClient) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	o, err := t.find(ctx)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return []Change{{Link: t.link, Op: "create", What: t.What(), Detail: "  name: " + seerrPrefix + t.name + "\n  host: " + t.host + t.base, Apply: func(ctx context.Context) error {
			return t.a.do(ctx, "POST", "/download_client", t.body(seerrPrefix+t.name), nil)
		}}}, nil
	}
	name := fmt.Sprint(o["name"])
	op, d := "update", []string{}
	body := t.body(name)
	if !strings.HasPrefix(name, seerrPrefix) {
		if !adopt {
			return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: fmt.Sprintf("%q exists without the %s prefix", name, strings.TrimSpace(seerrPrefix))}}, nil
		}
		op = "adopt"
		body["name"] = seerrPrefix + name
		d = append(d, fmt.Sprintf("  name: %s -> %s", name, body["name"]))
	}
	if o["enabled"] != true {
		d = append(d, "  enabled: true")
	}
	if strings.Trim(fmt.Sprint(o["urlBase"]), "/") != strings.Trim(t.base, "/") {
		d = append(d, fmt.Sprintf("  urlBase: %v -> %s", o["urlBase"], t.base))
	}
	if op == "update" && len(d) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	id := fmt.Sprint(o["id"])
	return []Change{{Link: t.link, Op: op, What: t.What(), Detail: strings.Join(d, "\n"), Apply: func(ctx context.Context) error {
		return t.a.do(ctx, "PUT", "/download_client/"+id, body, nil)
	}}}, nil
}
