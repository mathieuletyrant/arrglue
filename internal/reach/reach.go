// Package reach computes, per caller -> callee pair, the URL the caller should
// use, and verifies it from arrglue's own vantage point when that vantage is
// equivalent to the caller's (same Docker network, or both on the host).
//
// ponytail: no ephemeral `--network container:<caller>` probe yet; pairs whose
// vantage arrglue cannot share are reported unverified.
package reach

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

type Result struct {
	From, To *graph.Instance
	URL      string // best candidate, with url_base, no trailing slash
	Verified bool
	Err      string // why verification failed, or "unverified: ..." when it could not run
}

// Candidates lists base URLs for from -> to, best first.
func Candidates(from, to *graph.Instance, app *catalog.App) []string {
	port := to.HTTPPort(app.Ports["http"])
	base := "/" + strings.Trim(to.Config["url_base"], "/")
	base = strings.TrimSuffix(base, "/")
	var c []string
	add := func(host string, port int) { c = append(c, fmt.Sprintf("http://%s:%d%s", host, port, base)) }
	toHost := len(to.Networks) > 0 && to.Networks[0] == "host"
	fromHost := len(from.Networks) > 0 && from.Networks[0] == "host"
	if !toHost && !fromHost && shared(from.Networks, to.Networks) {
		add(to.Name, port)
	}
	if fromHost {
		switch {
		case toHost:
			add("127.0.0.1", port)
		case to.PublishedPort(port) != 0:
			add("127.0.0.1", to.PublishedPort(port))
		}
	} else if toHost || to.PublishedPort(port) != 0 {
		p := port
		if !toHost {
			p = to.PublishedPort(port)
		}
		if from.Gateway != "" { // bridge gateway = the host, works on plain Linux
			add(from.Gateway, p)
		}
		add("host.docker.internal", p) // Docker Desktop / OrbStack, or --add-host host-gateway
	}
	return c
}

// Pairs lists the caller -> callee couples worth wiring: kinds linked in the
// catalog AND on a common Docker network (or either side in host/native mode).
// Keeps two stacks on one host from being cross-wired through published ports.
func Pairs(cat *catalog.Catalog, insts []graph.Instance) [][2]*graph.Instance {
	var out [][2]*graph.Instance
	for i := range insts {
		from := &insts[i]
		for _, kind := range cat.Apps[from.Kind].Links {
			for j := range insts {
				to := &insts[j]
				if to.Kind != kind {
					continue
				}
				fromHost := len(from.Networks) > 0 && from.Networks[0] == "host"
				toHost := len(to.Networks) > 0 && to.Networks[0] == "host"
				if fromHost || toHost || shared(from.Networks, to.Networks) {
					out = append(out, [2]*graph.Instance{from, to})
				}
			}
		}
	}
	return out
}

func shared(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y && x != "host" {
				return true
			}
		}
	}
	return false
}

// Probe calls the catalog health endpoint under base. Returns the version if reported.
func Probe(ctx context.Context, base string, app *catalog.App, key string) (string, error) {
	u := base + "/" + strings.TrimPrefix(app.Health.Path, "/")
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	if app.Health.Auth.Header != "" && key != "" {
		req.Header.Set(app.Health.Auth.Header, key)
	}
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	ok := app.Health.OK
	if len(ok) == 0 {
		ok = []int{200}
	}
	accepted := false
	for _, s := range ok {
		accepted = accepted || s == res.StatusCode
	}
	if !accepted {
		return "", fmt.Errorf("GET %s: %s", u, res.Status)
	}
	if app.Health.VersionField == "" || res.StatusCode != 200 {
		return "", nil
	}
	var body map[string]any
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if json.Unmarshal(b, &body) == nil && body[app.Health.VersionField] != nil {
		return fmt.Sprint(body[app.Health.VersionField]), nil
	}
	return "", nil
}

// Self probes `to` from arrglue's own vantage (self) and returns the working base URL.
func Self(ctx context.Context, self, to *graph.Instance, app *catalog.App) (url, version string, err error) {
	cands := Candidates(self, to, app)
	if len(cands) == 0 {
		return "", "", fmt.Errorf("unverified: no route from arrglue (networks %v) to %s (networks %v, no published port)", self.Networks, to.Name, to.Networks)
	}
	for _, c := range cands {
		if version, err = Probe(ctx, c, app, to.Secret.Value); err == nil {
			return c, version, nil
		}
	}
	return "", "", err
}

// Check verifies from -> to: the best candidate is probed only if arrglue
// would use the very same URL from its own vantage.
func Check(ctx context.Context, self, from, to *graph.Instance, app *catalog.App) Result {
	r := Result{From: from, To: to}
	cands := Candidates(from, to, app)
	if len(cands) == 0 {
		r.Err = fmt.Sprintf("no route: %s (networks %v) cannot reach %s (networks %v, no published port)", from.Name, from.Networks, to.Name, to.Networks)
		return r
	}
	r.URL = cands[0]
	for _, mine := range Candidates(self, to, app) {
		if mine != r.URL {
			continue
		}
		if _, err := Probe(ctx, r.URL, app, to.Secret.Value); err != nil {
			r.Err = err.Error()
		} else {
			r.Verified = true
		}
		return r
	}
	r.Err = "unverified: arrglue does not share " + from.Name + "'s vantage point"
	return r
}
