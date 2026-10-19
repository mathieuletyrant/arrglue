// Package docker discovers *arr instances through the Docker Engine API.
// It talks raw HTTP to the socket: no SDK, read-only endpoints only.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

type Client struct {
	HTTP *http.Client
	Base string // "http://docker" for a socket, or an http(s) URL
}

// New builds a client from DOCKER_HOST (unix:// or tcp://), default /var/run/docker.sock.
func New() *Client {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	if strings.HasPrefix(host, "tcp://") {
		return &Client{HTTP: http.DefaultClient, Base: "http://" + strings.TrimPrefix(host, "tcp://")}
	}
	sock := strings.TrimPrefix(host, "unix://")
	return &Client{Base: "http://docker", HTTP: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

// container is the subset of GET /containers/json we need.
type container struct {
	ID              string `json:"Id"`
	Names           []string
	Image           string
	Labels          map[string]string
	State           string
	HostConfig      struct{ NetworkMode string }
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string
			Gateway   string
			Aliases   []string
		}
	}
	Mounts []struct{ Source, Destination string }
	Ports  []struct {
		IP          string
		PrivatePort int
		PublicPort  int
		Type        string
	}
}

func (c *Client) list(ctx context.Context) ([]container, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/containers/json", nil)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("docker: %s", res.Status)
	}
	var out []container
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// Discover lists running containers and maps the known ones to Instances.
func (c *Client) Discover(ctx context.Context, cat *catalog.Catalog) ([]graph.Instance, error) {
	cs, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []graph.Instance
	for _, ct := range cs {
		if ct.Labels["arrglue.ignore"] == "true" {
			continue
		}
		app := cat.Apps[ct.Labels["arrglue.kind"]]
		if app == nil {
			app = cat.MatchImage(ct.Image)
		}
		if app == nil {
			continue
		}
		name := strings.TrimPrefix(ct.Names[0], "/")
		inst := graph.Instance{
			ID: "docker:" + name, Kind: app.Kind, Provider: "docker", Name: name,
			Labels: ct.Labels, Config: map[string]string{},
		}
		if ct.HostConfig.NetworkMode == "host" {
			inst.Networks = []string{"host"}
		}
		for n, net := range ct.NetworkSettings.Networks {
			inst.Networks = append(inst.Networks, n)
			inst.Aliases = append(inst.Aliases, net.Aliases...)
			if inst.Gateway == "" {
				inst.Gateway = net.Gateway
			}
		}
		sort.Strings(inst.Networks)
		seen := map[graph.Port]bool{} // same port is listed once per bound IP (v4, v6)
		for _, p := range ct.Ports {
			port := graph.Port{Internal: p.PrivatePort, Published: p.PublicPort}
			if p.Type == "tcp" && !seen[port] {
				seen[port] = true
				inst.Ports = append(inst.Ports, port)
			}
		}
		for _, m := range ct.Mounts {
			inst.Mounts = append(inst.Mounts, graph.Mount{App: m.Destination, Host: m.Source})
		}
		readConfig(&inst, app)
		readSecret(&inst, app)
		if inst.Secret.Value == "" && app.Secret.Env != "" {
			if v := c.env(ctx, ct.ID, app.Secret.Env); v != "" {
				inst.Secret = graph.SecretRef{Source: "env:" + app.Secret.Env, Value: v}
			}
		}
		for k, v := range app.Config.Defaults {
			if inst.Config[k] == "" {
				inst.Config[k] = v
			}
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// readConfig locates the app's config file through its mounts and extracts catalog fields.
func readConfig(inst *graph.Instance, app *catalog.App) {
	if len(app.Config.Paths) == 0 { // app without a readable config file
		return
	}
	f := ""
	for _, p := range app.Config.Paths {
		if h, ok := inst.ToHost(p); ok {
			if _, err := os.Stat(h); err == nil {
				f = h
				break
			}
		}
	}
	if f == "" {
		where := "no mount covers " + app.Config.Paths[0]
		if h, ok := inst.ToHost(app.Config.Paths[0]); ok {
			where = h + " missing or not readable from here"
		}
		inst.Errors = append(inst.Errors, "config not found: "+where)
		return
	}
	fields, err := app.ReadConfig(f)
	if err != nil {
		inst.Errors = append(inst.Errors, err.Error())
		return
	}
	inst.ConfigFile = f
	if k, ok := fields["api_key"]; ok {
		delete(fields, "api_key")
		inst.Secret = graph.SecretRef{Source: "file:" + f + "#api_key", Value: k}
	}
	inst.Config = fields
}

// env reads one environment variable of a container (GET /containers/{id}/json).
func (c *Client) env(ctx context.Context, id, name string) string {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/containers/"+id+"/json", nil)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var out struct{ Config struct{ Env []string } }
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		return ""
	}
	for _, e := range out.Config.Env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}

// readSecret fills the key from the app's database(s) when the config had
// none. Several candidate databases may exist (old and new layouts, stale
// copies left by migrations): every key found is kept as a candidate, best
// first (newest database first), and the health probe promotes the one the
// app actually accepts.
func readSecret(inst *graph.Instance, app *catalog.App) {
	if inst.Secret.Value != "" || app.Secret.SQLite == "" {
		return
	}
	type db struct {
		file string
		mod  time.Time
	}
	var dbs []db
	for _, p := range app.Secret.Paths {
		if h, ok := inst.ToHost(p); ok {
			if fi, err := os.Stat(h); err == nil {
				dbs = append(dbs, db{h, fi.ModTime()})
			}
		}
	}
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].mod.After(dbs[j].mod) })
	var lastErr error
	seen := map[string]bool{}
	for _, d := range dbs {
		keys, err := app.ReadSecrets(d.file)
		if err != nil {
			lastErr = err
			continue
		}
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				inst.Candidates = append(inst.Candidates, graph.SecretRef{Source: "sqlite:" + d.file, Value: k})
			}
		}
	}
	if len(inst.Candidates) > 0 {
		inst.Secret, inst.Candidates = inst.Candidates[0], inst.Candidates[1:]
	} else if lastErr != nil {
		inst.Errors = append(inst.Errors, lastErr.Error())
	}
}

// Events streams container/network event actions ("start", "die",
// "connect"...) until ctx ends. Each value is a hint to rediscover.
func (c *Client) Events(ctx context.Context) (<-chan string, error) {
	q := url.Values{"filters": {`{"type":{"container":true,"network":true}}`}}
	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/events?"+q.Encode(), nil)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker events: %w", err)
	}
	ch := make(chan string, 16)
	go func() {
		defer res.Body.Close()
		defer close(ch)
		dec := json.NewDecoder(res.Body)
		for {
			var ev struct{ Action string }
			if err := dec.Decode(&ev); err != nil {
				return
			}
			select {
			case ch <- ev.Action:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// Self describes arrglue's own vantage point: the container it runs in (found
// by hostname = container id), or a host-network pseudo instance when native.
func (c *Client) Self(ctx context.Context) *graph.Instance {
	self := &graph.Instance{ID: "arrglue", Name: "arrglue", Networks: []string{"host"}}
	host, _ := os.Hostname()
	if _, err := os.Stat("/.dockerenv"); err != nil || host == "" {
		return self
	}
	cs, err := c.list(ctx)
	if err != nil {
		return self
	}
	for _, ct := range cs {
		if strings.HasPrefix(ct.ID, host) {
			self.Name = strings.TrimPrefix(ct.Names[0], "/")
			self.Networks = nil
			if ct.HostConfig.NetworkMode == "host" {
				self.Networks = []string{"host"}
			}
			for n, net := range ct.NetworkSettings.Networks {
				self.Networks = append(self.Networks, n)
				if self.Gateway == "" {
					self.Gateway = net.Gateway
				}
			}
			sort.Strings(self.Networks)
		}
	}
	return self
}
