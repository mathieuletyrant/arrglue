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
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

const Tag = "arrglue"

// Pending is a Test error that is a warning, not a failure: the link cannot
// be configured yet for a reason the user has to act on.
type Pending string

func (p Pending) Error() string { return string(p) }

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
	Op     string // create | update | adopt | noop | skip | manual (a file the user must edit)
	What   string
	Detail string                          // diff or reason
	Apply  func(ctx context.Context) error // nil when nothing to do
}

type Connector interface {
	Matches(from, to *graph.Instance) bool
	// Target describes the object to maintain in `from`, or a reason to skip.
	Target(env *Env, from, to *graph.Instance) (Target, string)
}

// Target is one object a connector maintains in the caller's API.
type Target interface {
	What() string
	Plan(ctx context.Context, adopt bool) ([]Change, error)
	Test(ctx context.Context) (found bool, err error) // ask the caller to test the existing object
}

var All = []Connector{prowlarr{}, downloadClient{}, seerr{}, mediaNotify{}, maintainerr{}, healarr{}, cleanuparr{}, spotarr{}, crossSeed{}, bazarr{}, lingarr{}, profilarr{}}

// servarrTarget is a Servarr-style object (download client, application...):
// {implementation, fields: [{name, value}], tags}, with /schema and /test.
type servarrTarget struct {
	API            api
	Path           string // collection path, e.g. "/applications"
	Implementation string
	Link, Desc     string
	Identity       func(o obj) bool                            // is this existing object ours (regardless of tag)?
	Fill           func(o obj, force bool) []string            // set desired fields, return diff lines
	Extra          func(ctx context.Context) ([]Change, error) // dependent objects (remote path mappings), optional
}

func (t *servarrTarget) What() string { return t.Desc }

// Plan computes the changes for a target: the main object, then extras.
func (t *servarrTarget) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	ch, err := t.upsert(ctx, adopt)
	if err != nil || t.Extra == nil {
		return ch, err
	}
	more, err := t.Extra(ctx)
	return append(ch, more...), err
}

// Test asks the caller's API to test the existing object (connection + key).
// found=false when the object does not exist yet.
func (t *servarrTarget) Test(ctx context.Context) (found bool, err error) {
	o, err := t.find(ctx)
	if err != nil || o == nil {
		return false, err
	}
	return true, t.testObj(ctx, o)
}

func (t *servarrTarget) testObj(ctx context.Context, o obj) error {
	var raw json.RawMessage // success: the object; 400: a list of validation failures
	err := t.API.do(ctx, "POST", t.Path+"/test", o, &raw)
	var failures []struct{ ErrorMessage string }
	if err != nil && json.Unmarshal(raw, &failures) == nil && len(failures) > 0 {
		var msgs []string
		for _, f := range failures {
			msgs = append(msgs, f.ErrorMessage)
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return err
}

func (t *servarrTarget) find(ctx context.Context) (obj, error) {
	var existing []obj
	if err := t.API.do(ctx, "GET", t.Path, nil, &existing); err != nil {
		return nil, err
	}
	for _, o := range existing {
		if o["implementation"] == t.Implementation && t.Identity(o) {
			return o, nil
		}
	}
	return nil, nil
}

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

// masked: APIs hide secrets on read as "********" (Servarr) or "••••••" (Cleanuparr).
func masked(v any) bool {
	s, ok := v.(string)
	return ok && s != "" && strings.Trim(s, "*•") == ""
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

// api is a minimal JSON client: base URL + path prefix + auth header.
type api struct {
	base, prefix string
	headers      map[string]string
}

// newAPI targets a Servarr-style API (/api/vN, X-Api-Key).
func newAPI(base string, app *catalog.App, key string) api {
	v := "v3"
	if len(app.API.Versions) > 0 {
		v = app.API.Versions[0]
	}
	return api{base: base, prefix: "/api/" + v, headers: map[string]string{"X-Api-Key": key}}
}

// jsonAPI targets any JSON API with one auth header (e.g. "Authorization", "Bearer "+token).
func jsonAPI(base, prefix, header, value string) api {
	return api{base: base, prefix: prefix, headers: map[string]string{header: value}}
}

func (a api) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, a.base+a.prefix+path, body)
	for k, v := range a.headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if os.Getenv("ARRGLUE_DEBUG") != "" {
		log.Printf("%6dms %s %s", time.Since(start).Milliseconds(), method, a.base+a.prefix+path)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode/100 != 2 {
		if out != nil && res.StatusCode == 400 {
			json.Unmarshal(b, out) // validation failures, best effort
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// form posts application/x-www-form-urlencoded values (Bazarr saves settings that way).
func (a api) form(ctx context.Context, path string, values url.Values) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", a.base+a.prefix+path, strings.NewReader(values.Encode()))
	for k, v := range a.headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: %s: %s", path, res.Status, strings.TrimSpace(string(b)))
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
// then noop / update / skip (unmanaged) / create. A managed object that fails
// its API test gets all desired fields re-sent (secrets are masked on read).
func (t *servarrTarget) upsert(ctx context.Context, adopt bool) ([]Change, error) {
	a, link, what := t.API, t.Link, t.Desc
	o, err := t.find(ctx)
	if err != nil {
		return nil, err
	}
	tag, err := a.tagID(ctx, false)
	if err != nil {
		return nil, err
	}
	if o != nil {
		op, force, why := "update", false, ""
		if !o.hasTag(tag) {
			if !adopt {
				return []Change{{Link: link, Op: "skip", What: what, Detail: fmt.Sprintf("%q exists without the %s tag", o["name"], Tag)}}, nil
			}
			op, force = "adopt", true
		} else if err := t.testObj(ctx, o); err != nil {
			force, why = true, "  (re-sent: fails its test: "+err.Error()+")"
		}
		diff := t.Fill(o, force)
		if op == "update" && len(diff) == 0 {
			return []Change{{Link: link, Op: "noop", What: what}}, nil
		}
		if why != "" {
			diff = append(diff, why)
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
			return a.do(ctx, "PUT", t.Path+"/"+id, o, nil)
		}}}, nil
	}
	o, err = a.schema(ctx, t.Path, t.Implementation)
	if err != nil {
		return nil, err
	}
	diff := t.Fill(o, true)
	sort.Strings(diff)
	return []Change{{Link: link, Op: "create", What: what, Detail: strings.Join(diff, "\n"), Apply: func(ctx context.Context) error {
		tag, err := a.tagID(ctx, true)
		if err != nil {
			return err
		}
		o["tags"] = []any{tag}
		return a.do(ctx, "POST", t.Path, o, nil)
	}}}, nil
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// hostPort splits a base URL into host, port and path.
func hostPort(raw string) (host string, port int, path string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, ""
	}
	host, p, _ := strings.Cut(u.Host, ":")
	port, _ = strconv.Atoi(p)
	return host, port, u.Path
}

// slot is a single-valued setting (one Seerr URL, one Jellyfin server...) in
// an app that has no list of servers: create when empty, skip when set by
// hand unless adopting, update when ours.
// ponytail: ownership of a slot cannot be marked; "set by hand" = set to
// something else. --adopt overwrites it.
func slot(link, what string, current map[string]any, want map[string]any, adopt bool, save func(ctx context.Context) error) Change {
	empty := true
	var d []string
	for k, v := range want {
		cur := current[k]
		if cur != nil && cur != "" {
			empty = false
		}
		if fmt.Sprint(cur) != fmt.Sprint(v) {
			if strings.Contains(strings.ToLower(k), "key") {
				d = append(d, "  "+k+": *** -> ***")
			} else {
				d = append(d, fmt.Sprintf("  %s: %v -> %v", k, cur, v))
			}
		}
	}
	sort.Strings(d)
	switch {
	case len(d) == 0:
		return Change{Link: link, Op: "noop", What: what}
	case empty:
		return Change{Link: link, Op: "create", What: what, Detail: strings.Join(d, "\n"), Apply: save}
	case adopt:
		return Change{Link: link, Op: "adopt", What: what, Detail: strings.Join(d, "\n"), Apply: save}
	}
	return Change{Link: link, Op: "skip", What: what, Detail: fmt.Sprintf("set by hand to %v; --adopt overwrites it", current[firstKey(want)])}
}

func firstKey(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.Contains(strings.ToLower(k), "key") {
			return k
		}
	}
	return keys[0]
}
