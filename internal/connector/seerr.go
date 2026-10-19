package connector

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// seerr -> radarr/sonarr: a server entry in Seerr's settings (profile and
// root folder picked from what the app offers, kept if already set).
// seerr -> jellyfin: Seerr's media server host, checked and corrected, but the
// first-time wizard stays manual (it needs a Jellyfin login).
type seerr struct{}

const seerrPrefix = "[arrglue] " // Seerr has no tags: ownership is a name prefix

func (seerr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "seerr" && (to.Kind == "radarr" || to.Kind == "sonarr" || to.Kind == "jellyfin")
}

func (seerr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	u, _ := url.Parse(toURL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	a := newAPI(env.URL(nil, from), env.Cat.Apps[from.Kind], from.Secret.Value)
	if to.Kind == "jellyfin" {
		return &seerrMedia{a: a, link: link, host: host, port: port, base: u.Path}, ""
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	return &seerrArr{a: a, link: link, kind: to.Kind, name: to.Name, host: host, port: port, base: u.Path, key: to.Secret.Value}, ""
}

type seerrArr struct {
	a                api
	link, kind, name string
	host, base, key  string
	port             int
}

func (t *seerrArr) What() string { return fmt.Sprintf("%s server %q", t.kind, t.name) }

func (t *seerrArr) find(ctx context.Context) ([]obj, obj, error) {
	var list []obj
	if err := t.a.do(ctx, "GET", "/settings/"+t.kind, nil, &list); err != nil {
		return nil, nil, err
	}
	for _, o := range list {
		if o["hostname"] == t.host && fmt.Sprint(o["port"]) == fmt.Sprint(t.port) {
			return list, o, nil
		}
	}
	return list, nil, nil
}

// test asks Seerr to connect to the app; returns the profiles and root folders it sees.
func (t *seerrArr) test(ctx context.Context, key string) (obj, error) {
	var res obj
	err := t.a.do(ctx, "POST", "/settings/"+t.kind+"/test", obj{"hostname": t.host, "port": t.port, "apiKey": key, "useSsl": false, "baseUrl": t.base}, &res)
	return res, err
}

const notSetUp = "Seerr is not set up yet: finish its setup wizard once (it needs your Jellyfin login), arrglue takes over the rest"

// initialized reports whether Seerr's first-run wizard is done (its API refuses everything before).
func initialized(ctx context.Context, a api) (bool, error) {
	var pub struct{ Initialized bool }
	err := a.do(ctx, "GET", "/settings/public", nil, &pub)
	return pub.Initialized, err
}

func (t *seerrArr) Test(ctx context.Context) (bool, error) {
	if ok, err := initialized(ctx, t.a); err != nil {
		return false, err
	} else if !ok {
		return false, Pending(notSetUp)
	}
	_, o, err := t.find(ctx)
	if err != nil || o == nil {
		return false, err
	}
	key, _ := o["apiKey"].(string)
	_, err = t.test(ctx, key)
	return true, err
}

func (t *seerrArr) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	if ok, err := initialized(ctx, t.a); err != nil {
		return nil, err
	} else if !ok {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: notSetUp}}, nil
	}
	list, o, err := t.find(ctx)
	if err != nil {
		return nil, err
	}
	create := o == nil
	if !create && !strings.HasPrefix(fmt.Sprint(o["name"]), seerrPrefix) && !adopt {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: fmt.Sprintf("%q exists but is not managed by arrglue (no %q prefix); left untouched, use -adopt to take it over", o["name"], strings.TrimSpace(seerrPrefix))}}, nil
	}
	seen, err := t.test(ctx, t.key) // what the app offers, through Seerr
	if err != nil {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: "Seerr cannot reach " + t.name + ": " + err.Error()}}, nil
	}
	var d []string
	op := "update"
	if create {
		op = "create"
		o = obj{"name": seerrPrefix + t.name, "is4k": false, "tags": []any{}, "syncEnabled": true, "preventSearch": false, "externalUrl": ""}
		d = append(d, "  name: "+seerrPrefix+t.name)
		if t.kind == "radarr" {
			o["minimumAvailability"] = "released"
		} else {
			o["enableSeasonFolders"] = true
		}
	} else if !strings.HasPrefix(fmt.Sprint(o["name"]), seerrPrefix) {
		op = "adopt"
		d = append(d, fmt.Sprintf("  name: %v -> %s%v", o["name"], seerrPrefix, o["name"]))
		o["name"] = seerrPrefix + fmt.Sprint(o["name"])
	}
	want := map[string]any{"hostname": t.host, "port": t.port, "useSsl": false, "baseUrl": t.base}
	for k, v := range want {
		if fmt.Sprint(o[k]) != fmt.Sprint(v) {
			d = append(d, fmt.Sprintf("  %s: %v -> %v", k, o[k], v))
			o[k] = v
		}
	}
	if cur, _ := o["apiKey"].(string); cur != t.key && !(op == "update" && masked(cur)) {
		d = append(d, "  apiKey: *** -> ***")
		o["apiKey"] = t.key
	}
	// profile and root folder: keep what is set if the app still offers it, else first offered
	profiles, _ := seen["profiles"].([]any)
	if len(profiles) == 0 {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: t.name + " has no quality profile yet"}}, nil
	}
	roots, _ := seen["rootFolders"].([]any)
	if len(roots) == 0 {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: t.name + " has no root folder yet"}}, nil
	}
	if !has(profiles, "id", o["activeProfileId"]) {
		p := profiles[0].(map[string]any)
		d = append(d, fmt.Sprintf("  profile: %v -> %v", o["activeProfileName"], p["name"]))
		o["activeProfileId"], o["activeProfileName"] = p["id"], p["name"]
	}
	if !has(roots, "path", o["activeDirectory"]) {
		r := roots[0].(map[string]any)
		d = append(d, fmt.Sprintf("  rootFolder: %v -> %v", o["activeDirectory"], r["path"]))
		o["activeDirectory"] = r["path"]
	}
	if _, set := o["isDefault"]; !set {
		def := true
		for _, other := range list {
			if other["isDefault"] == true && other["is4k"] == o["is4k"] {
				def = false
			}
		}
		o["isDefault"] = def
	}
	if op == "update" && len(d) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	id := strings.TrimSuffix(fmt.Sprint(o["id"]), ".0")
	return []Change{{Link: t.link, Op: op, What: t.What(), Detail: strings.Join(d, "\n"), Apply: func(ctx context.Context) error {
		if create {
			return t.a.do(ctx, "POST", "/settings/"+t.kind, o, nil)
		}
		return t.a.do(ctx, "PUT", "/settings/"+t.kind+"/"+id, o, nil)
	}}}, nil
}

func has(list []any, field string, v any) bool {
	for _, x := range list {
		if m, ok := x.(map[string]any); ok && fmt.Sprint(m[field]) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

// seerrMedia keeps Seerr's Jellyfin host in line; setup itself stays manual.
type seerrMedia struct {
	a          api
	link       string
	host, base string
	port       int
}

func (t *seerrMedia) What() string { return "media server jellyfin" }

func (t *seerrMedia) current(ctx context.Context) (obj, error) {
	if ok, err := initialized(ctx, t.a); err != nil || !ok {
		return nil, err
	}
	var o obj
	return o, t.a.do(ctx, "GET", "/settings/jellyfin", nil, &o)
}

// want is the desired media server entry. Seerr's settings object calls the
// host "ip" (only its auth wizard says "hostname").
func (t *seerrMedia) want() map[string]any {
	return map[string]any{"ip": t.host, "port": t.port, "useSsl": false, "urlBase": t.base}
}

func (t *seerrMedia) diff(o obj) []string {
	var d []string
	for _, k := range []string{"ip", "port", "useSsl", "urlBase"} {
		if v := t.want()[k]; fmt.Sprint(o[k]) != fmt.Sprint(v) {
			d = append(d, fmt.Sprintf("  %s: %v -> %v", k, o[k], v))
		}
	}
	return d
}

func (t *seerrMedia) Test(ctx context.Context) (bool, error) {
	o, err := t.current(ctx)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, Pending(notSetUp)
	}
	if d := t.diff(o); len(d) > 0 {
		return true, fmt.Errorf("Seerr points elsewhere:%s", strings.ReplaceAll(strings.Join(d, ","), "\n", ""))
	}
	return true, nil
}

func (t *seerrMedia) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	o, err := t.current(ctx)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: notSetUp}}, nil
	}
	d := t.diff(o)
	if len(d) == 0 {
		return []Change{{Link: t.link, Op: "noop", What: t.What()}}, nil
	}
	for k, v := range t.want() {
		o[k] = v
	}
	return []Change{{Link: t.link, Op: "update", What: t.What(), Detail: strings.Join(d, "\n"), Apply: func(ctx context.Context) error {
		return t.a.do(ctx, "PUT", "/settings/jellyfin", o, nil)
	}}}, nil
}
