package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// maintainerr -> radarr/sonarr (server list), jellyfin and seerr (single slots).
// Maintainerr's API has no auth and no tags: list entries are owned through a
// name prefix, slots through slot().
type maintainerr struct{}

func (maintainerr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "maintainerr" && (to.Kind == "radarr" || to.Kind == "sonarr" || to.Kind == "jellyfin" || to.Kind == "seerr")
}

func (maintainerr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	a := jsonAPI(env.URL(nil, from), "/api/settings", "", "")
	switch to.Kind {
	case "jellyfin":
		// Maintainerr tracks watch status for one Jellyfin user: the first administrator
		uid, name, err := jellyfinAdmin(env.URL(nil, to), to.Secret.Value)
		if err != nil {
			return nil, "cannot list Jellyfin users: " + err.Error()
		}
		return &maintainerrSlot{a: a, link: link, what: "media server jellyfin (user " + name + ")", path: "/jellyfin", test: "/jellyfin/test",
			want: map[string]any{"jellyfin_url": toURL, "jellyfin_api_key": to.Secret.Value, "jellyfin_user_id": uid}}, ""
	case "seerr":
		return &maintainerrSlot{a: a, link: link, what: "seerr", path: "/seerr", test: "/test/seerr",
			want: map[string]any{"url": toURL, "api_key": to.Secret.Value}}, ""
	}
	return &maintainerrArr{a: a, link: link, kind: to.Kind, name: to.Name, url: toURL, key: to.Secret.Value}, ""
}

// Maintainerr wraps answers as {status: OK|NOK, code, message, data}.
type mResp struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    obj    `json:"data"`
}

func (r mResp) err() error {
	if r.Status == "NOK" {
		return fmt.Errorf("%s", r.Message)
	}
	return nil
}

type maintainerrArr struct {
	a                          api
	link, kind, name, url, key string
}

func (t *maintainerrArr) What() string { return fmt.Sprintf("%s server %q", t.kind, t.name) }

func (t *maintainerrArr) find(ctx context.Context) (obj, error) {
	var list []obj
	if err := t.a.do(ctx, "GET", "/"+t.kind, nil, &list); err != nil {
		return nil, err
	}
	for _, o := range list {
		if sameEndpoint(fmt.Sprint(o["url"]), t.url) {
			return o, nil
		}
	}
	return nil, nil
}

func (t *maintainerrArr) Test(ctx context.Context) (bool, error) {
	o, err := t.find(ctx)
	if err != nil || o == nil {
		return false, err
	}
	var r mResp
	if err := t.a.do(ctx, "POST", "/test/"+t.kind, obj{"serverName": o["serverName"], "url": o["url"], "apiKey": o["apiKey"]}, &r); err != nil {
		return true, err
	}
	return true, r.err()
}

func (t *maintainerrArr) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	o, err := t.find(ctx)
	if err != nil {
		return nil, err
	}
	body := obj{"serverName": seerrPrefix + t.name, "url": t.url, "apiKey": t.key}
	if o == nil {
		return []Change{{Link: t.link, Op: "create", What: t.What(), Detail: "  serverName: " + seerrPrefix + t.name + "\n  url: " + t.url, Apply: func(ctx context.Context) error {
			var r mResp
			if err := t.a.do(ctx, "POST", "/"+t.kind, body, &r); err != nil {
				return err
			}
			return r.err()
		}}}, nil
	}
	op := "update"
	name := fmt.Sprint(o["serverName"])
	if !strings.HasPrefix(name, seerrPrefix) {
		if !adopt {
			return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: fmt.Sprintf("%q exists without the %s prefix", name, strings.TrimSpace(seerrPrefix))}}, nil
		}
		op = "adopt"
		body["serverName"] = seerrPrefix + name
	} else {
		body["serverName"] = name
	}
	var d []string
	if op == "adopt" {
		d = append(d, fmt.Sprintf("  serverName: %s -> %s", name, body["serverName"]))
	}
	if o["apiKey"] != t.key {
		d = append(d, "  apiKey: *** -> ***")
	}
	if op == "update" && len(d) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	id := strings.TrimSuffix(fmt.Sprint(o["id"]), ".0")
	return []Change{{Link: t.link, Op: op, What: t.What(), Detail: strings.Join(d, "\n"), Apply: func(ctx context.Context) error {
		var r mResp
		if err := t.a.do(ctx, "PUT", "/"+t.kind+"/"+id, body, &r); err != nil {
			return err
		}
		return r.err()
	}}}, nil
}

type maintainerrSlot struct {
	a                      api
	link, what, path, test string
	want                   map[string]any
}

func (t *maintainerrSlot) What() string { return t.what }

func (t *maintainerrSlot) Test(ctx context.Context) (bool, error) {
	var cur obj
	if err := t.a.do(ctx, "GET", t.path, nil, &cur); err != nil {
		return false, err
	}
	set := false
	for k := range t.want {
		if cur[k] != nil && cur[k] != "" {
			set = true
		}
	}
	if !set {
		return false, nil
	}
	var r mResp
	if err := t.a.do(ctx, "POST", t.test, cur, &r); err != nil {
		return true, err
	}
	return true, r.err()
}

func (t *maintainerrSlot) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	var cur obj
	if err := t.a.do(ctx, "GET", t.path, nil, &cur); err != nil {
		return nil, err
	}
	return []Change{slot(t.link, t.what, cur, t.want, adopt, func(ctx context.Context) error {
		body := obj{}
		for k, v := range cur {
			body[k] = v
		}
		for k, v := range t.want {
			body[k] = v
		}
		var r mResp
		if err := t.a.do(ctx, "POST", t.path, body, &r); err != nil {
			return err
		}
		return r.err()
	})}, nil
}

// jellyfinAdmin returns the id and name of Jellyfin's first administrator.
func jellyfinAdmin(base, key string) (id, name string, err error) {
	var users []struct {
		Id, Name string
		Policy   struct{ IsAdministrator bool }
	}
	if err := jsonAPI(base, "", "X-Emby-Token", key).do(context.Background(), "GET", "/Users", nil, &users); err != nil {
		return "", "", err
	}
	for _, u := range users {
		if u.Policy.IsAdministrator {
			return u.Id, u.Name, nil
		}
	}
	if len(users) > 0 {
		return users[0].Id, users[0].Name, nil
	}
	return "", "", fmt.Errorf("no user")
}
