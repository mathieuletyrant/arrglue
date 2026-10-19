// Package connector knows how to create and maintain one kind of link between
// two apps through their APIs. Every object it creates carries the `arrglue`
// tag; equivalent objects created by hand are reported, never touched.
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

const Tag = "arrglue"

// Env is what connectors need from the engine: resolved URLs and directories.
type Env struct {
	Cat *catalog.Catalog
	// URL returns the base URL `from` should use to reach `to` ("" if no route).
	// from == nil means "from arrglue itself" (used to read/write APIs).
	URL   func(from, to *graph.Instance) string
	Dirs  map[string][]string // instance ID -> dirs it uses (see paths.Dirs)
	Adopt bool                // take over equivalent objects that lack the arrglue tag
}

type Change struct {
	Link   string // "prowlarr_server_1 -> radarr_server_1"
	Op     string // create | update | adopt | noop | skip
	What   string
	Detail string                          // diff or reason
	Apply  func(ctx context.Context) error // nil when nothing to do
}

type Connector interface {
	Matches(from, to *graph.Instance) bool
	Plan(ctx context.Context, env *Env, from, to *graph.Instance) ([]Change, error)
}

var All = []Connector{prowlarr{}, downloadClient{}}

// obj is a Servarr API object: {id, name, implementation, fields: [{name, value}], tags: [...]}.
type obj map[string]any

func (o obj) field(name string) any {
	fs, _ := o["fields"].([]any)
	for _, f := range fs {
		if m, ok := f.(map[string]any); ok && m["name"] == name {
			return m["value"]
		}
	}
	return nil
}

func (o obj) hasField(name string) bool {
	fs, _ := o["fields"].([]any)
	for _, f := range fs {
		if m, ok := f.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	return false
}

// set changes a field and returns a diff line if the value changed. Servarr
// APIs return secrets masked ("********"): a masked value is taken as
// unchanged unless force (adopt/create) is set.
// ponytail: stale keys are not detected on managed objects; use the API's
// /test endpoint in doctor if that ever matters.
func (o obj) set(name string, v any, force bool) string {
	fs, _ := o["fields"].([]any)
	for _, f := range fs {
		m, ok := f.(map[string]any)
		if !ok || m["name"] != name {
			continue
		}
		if fmt.Sprint(m["value"]) == fmt.Sprint(v) || (!force && masked(m["value"])) {
			return ""
		}
		old := m["value"]
		m["value"] = v
		if strings.Contains(strings.ToLower(name), "key") || strings.Contains(strings.ToLower(name), "password") {
			return fmt.Sprintf("  %s: *** -> ***", name)
		}
		return fmt.Sprintf("  %s: %v -> %v", name, old, v)
	}
	return ""
}

func masked(v any) bool {
	s, ok := v.(string)
	return ok && s != "" && strings.Trim(s, "*") == ""
}

func (o obj) hasTag(id float64) bool {
	ts, _ := o["tags"].([]any)
	for _, t := range ts {
		if t == id {
			return true
		}
	}
	return false
}

// api is a minimal Servarr-style JSON client.
type api struct {
	base, prefix, key string
}

func newAPI(base string, app *catalog.App, key string) api {
	v := "v3"
	if len(app.API.Versions) > 0 {
		v = app.API.Versions[0]
	}
	return api{base: base, prefix: "/api/" + v, key: key}
}

func (a api) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, a.base+a.prefix+path, body)
	req.Header.Set("X-Api-Key", a.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// tagID looks up the arrglue tag; creates it when create is true. -1 if absent.
func (a api) tagID(ctx context.Context, create bool) (float64, error) {
	var tags []obj
	if err := a.do(ctx, "GET", "/tag", nil, &tags); err != nil {
		return -1, err
	}
	for _, t := range tags {
		if t["label"] == Tag {
			return t["id"].(float64), nil
		}
	}
	if !create {
		return -1, nil
	}
	var t obj
	if err := a.do(ctx, "POST", "/tag", obj{"label": Tag}, &t); err != nil {
		return -1, err
	}
	return t["id"].(float64), nil
}

// schema returns the API's template object for an implementation.
func (a api) schema(ctx context.Context, path, implementation string) (obj, error) {
	var all []obj
	if err := a.do(ctx, "GET", path+"/schema", nil, &all); err != nil {
		return nil, err
	}
	for _, s := range all {
		if s["implementation"] == implementation {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%s: no implementation %q in schema", path, implementation)
}

// upsert is the shared plan logic: find an existing object by identity,
// then noop / update / skip (unmanaged) / create.
func upsert(ctx context.Context, a api, adopt bool, path, implementation string, link, what string,
	identity func(o obj) bool, fill func(o obj, force bool) []string) ([]Change, error) {
	var existing []obj
	if err := a.do(ctx, "GET", path, nil, &existing); err != nil {
		return nil, err
	}
	tag, err := a.tagID(ctx, false)
	if err != nil {
		return nil, err
	}
	for _, o := range existing {
		if o["implementation"] != implementation || !identity(o) {
			continue
		}
		op := "update"
		if !o.hasTag(tag) {
			if !adopt {
				return []Change{{Link: link, Op: "skip", What: what, Detail: fmt.Sprintf("%q exists but is not managed by arrglue (no %s tag); left untouched, use -adopt to take it over", o["name"], Tag)}}, nil
			}
			op = "adopt"
		}
		diff := fill(o, op == "adopt")
		if op == "update" && len(diff) == 0 {
			return []Change{{Link: link, Op: "noop", What: what}}, nil
		}
		id := strings.TrimSuffix(fmt.Sprint(o["id"]), ".0")
		return []Change{{Link: link, Op: op, What: what, Detail: strings.Join(diff, "\n"), Apply: func(ctx context.Context) error {
			if op == "adopt" {
				tag, err := a.tagID(ctx, true)
				if err != nil {
					return err
				}
				ts, _ := o["tags"].([]any)
				o["tags"] = append(ts, tag)
			}
			return a.do(ctx, "PUT", path+"/"+id, o, nil)
		}}}, nil
	}
	o, err := a.schema(ctx, path, implementation)
	if err != nil {
		return nil, err
	}
	diff := fill(o, true)
	sort.Strings(diff)
	return []Change{{Link: link, Op: "create", What: what, Detail: strings.Join(diff, "\n"), Apply: func(ctx context.Context) error {
		tag, err := a.tagID(ctx, true)
		if err != nil {
			return err
		}
		o["tags"] = []any{tag}
		return a.do(ctx, "POST", path, o, nil)
	}}}, nil
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
