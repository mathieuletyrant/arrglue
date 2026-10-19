// Package graph defines the normalized model shared by providers, resolvers and connectors.
package graph

import "strings"

type Instance struct {
	ID       string // stable: "docker:radarr"
	Kind     string // catalog kind
	Version  string
	Provider string
	Name     string            // DNS name on its networks (container name)
	Networks []string          // docker networks; ["host"] for host mode
	Aliases  []string          // extra DNS names
	Ports    []Port            // listening ports
	Mounts   []Mount           // app path -> host path
	Config   map[string]string // catalog fields found in config (secrets removed)
	Secret   SecretRef
	Labels   map[string]string
	Errors   []string // discovery problems (config unreadable, ...)
}

type Port struct {
	Internal  int
	Published int // 0 if not published on the host
}

type Mount struct{ App, Host string }

type SecretRef struct {
	Source string // where the key was found, e.g. "file:/host/radarr/config.xml#api_key"
	Value  string `json:"-" yaml:"-"`
}

// HTTPPort is the app's internal HTTP port: from config, else from catalog default.
func (i *Instance) HTTPPort(def int) int {
	if p := i.Config["port"]; p != "" {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return def
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return def
}

// PublishedPort returns the host port mapped to internal port p (0 if none).
func (i *Instance) PublishedPort(p int) int {
	for _, port := range i.Ports {
		if port.Internal == p {
			return port.Published
		}
	}
	return 0
}

// ToHost translates a path as seen by the app into a host path, using the
// longest matching mount. ok=false if no mount covers it.
func (i *Instance) ToHost(p string) (string, bool) {
	best := Mount{}
	for _, m := range i.Mounts {
		if (p == m.App || strings.HasPrefix(p, strings.TrimSuffix(m.App, "/")+"/")) && len(m.App) > len(best.App) {
			best = m
		}
	}
	if best.App == "" {
		return "", false
	}
	return best.Host + strings.TrimPrefix(p, strings.TrimSuffix(best.App, "/")), true
}
