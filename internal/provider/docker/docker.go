// Package docker discovers *arr instances through the Docker Engine API.
// It talks raw HTTP to the socket: no SDK, read-only endpoints only.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

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
	if k, ok := fields["api_key"]; ok {
		delete(fields, "api_key")
		inst.Secret = graph.SecretRef{Source: "file:" + f + "#api_key", Value: k}
	}
	inst.Config = fields
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
