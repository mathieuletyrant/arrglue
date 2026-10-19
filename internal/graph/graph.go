// Package graph defines the normalized model shared by providers, resolvers and connectors.
package graph

import "strings"

type Instance struct {
	ID         string // stable: "docker:radarr"
	Kind       string // catalog kind
	Version    string
	Provider   string
	Name       string            // DNS name on its networks (container name)
	Networks   []string          // docker networks; ["host"] for host mode
	Gateway    string            // gateway of the first docker network (route to the host)
	Aliases    []string          // extra DNS names
	Ports      []Port            // listening ports
	Mounts     []Mount           // app path -> host path
	Config     map[string]string // catalog fields found in config (secrets removed)
	ConfigFile string            // host path of the config file that was read, "" if none
	Secret     SecretRef
	Labels     map[string]string
	Errors     []string // discovery problems (config unreadable, ...)
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

// MountFor returns the longest mount whose app path covers p (nil if none).
func (i *Instance) MountFor(p string) *Mount {
	var best *Mount
	for k := range i.Mounts {
		m := &i.Mounts[k]
		if under(p, m.App) && (best == nil || len(m.App) > len(best.App)) {
			best = m
		}
	}
	return best
}

// ToHost translates a path as seen by the app into a host path.
func (i *Instance) ToHost(p string) (string, bool) {
	m := i.MountFor(p)
	if m == nil {
		return "", false
	}
	return join(m.Host, m.App, p), true
}

// FromHost translates a host path into the path the app sees, ok=false if the app cannot see it.
func (i *Instance) FromHost(h string) (string, bool) {
	var best *Mount
	for k := range i.Mounts {
		m := &i.Mounts[k]
		if under(h, m.Host) && (best == nil || len(m.Host) > len(best.Host)) {
			best = m
		}
	}
	if best == nil {
		return "", false
	}
	return join(best.App, best.Host, h), true
}

// Translate maps a path seen by a into the same path seen by b.
func Translate(a, b *Instance, p string) (string, bool) {
	h, ok := a.ToHost(p)
	if !ok {
		return "", false
	}
	return b.FromHost(h)
}

func under(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// join swaps prefix `from` of p for `to`.
func join(to, from, p string) string {
	return strings.TrimSuffix(to, "/") + strings.TrimPrefix(p, strings.TrimSuffix(from, "/"))
}
