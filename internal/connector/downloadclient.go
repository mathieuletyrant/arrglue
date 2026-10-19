package connector

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// radarr/sonarr -> transmission: a download client (category = app kind)
// plus a remote path mapping when the two apps see the download dir differently.
type downloadClient struct{}

func (downloadClient) Matches(from, to *graph.Instance) bool {
	return (from.Kind == "radarr" || from.Kind == "sonarr") && to.Kind == "transmission"
}

func (downloadClient) Plan(ctx context.Context, env *Env, from, to *graph.Instance) ([]Change, error) {
	link := from.Name + " -> " + to.Name
	what := fmt.Sprintf("download client Transmission (%s) in %s", to.Name, from.Name)
	clientURL := env.URL(from, to)
	if clientURL == "" {
		return []Change{{Link: link, Op: "skip", What: what, Detail: "no route between the two apps (see doctor)"}}, nil
	}
	if to.Config["auth_required"] == "true" {
		return []Change{{Link: link, Op: "skip", What: what, Detail: to.Name + " requires RPC credentials and arrglue has none"}}, nil
	}
	u, _ := url.Parse(clientURL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	a := newAPI(env.URL(nil, from), env.Cat.Apps[from.Kind], from.Secret.Value)
	identity := func(o obj) bool {
		return fmt.Sprint(o.field("host")) == host && fmt.Sprint(o.field("port")) == fmt.Sprint(port)
	}
	fill := func(o obj, force bool) []string {
		var d []string
		if o["name"] == nil || o["name"] == "" { // keep hand-given names on adopt
			d = append(d, "  name: "+to.Name)
			o["name"] = to.Name
		}
		if o["enable"] != true {
			d = append(d, "  enable: true")
			o["enable"] = true
		}
		want := map[string]any{"host": host, "port": port, "useSsl": false, "urlBase": u.Path + "/"}
		for _, cat := range []string{"movieCategory", "tvCategory"} { // category_from_kind, unless already set
			if v := o.field(cat); o.hasField(cat) && (v == nil || v == "") {
				want[cat] = from.Kind
			}
		}
		for k, v := range want {
			if l := o.set(k, v, force); l != "" {
				d = append(d, l)
			}
		}
		return d
	}
	changes, err := upsert(ctx, a, env.Adopt, "/downloadclient", "Transmission", link, what, identity, fill)
	if err != nil {
		return nil, err
	}
	// remote path mappings, one per download dir the two apps name differently
	for _, dir := range env.Dirs[to.ID] {
		seen, ok := graph.Translate(to, from, dir)
		if !ok || seen == dir {
			continue
		}
		remote, local := strings.TrimSuffix(dir, "/")+"/", strings.TrimSuffix(seen, "/")+"/"
		whatM := fmt.Sprintf("remote path mapping %s:%s -> %s in %s", host, remote, local, from.Name)
		var maps []obj
		if err := a.do(ctx, "GET", "/remotepathmapping", nil, &maps); err != nil {
			return nil, err
		}
		found := false
		for _, m := range maps {
			if m["host"] == host && m["remotePath"] == remote {
				found = true
				if m["localPath"] != local {
					changes = append(changes, Change{Link: link, Op: "skip", What: whatM, Detail: fmt.Sprintf("mapping exists with localPath %v; left untouched", m["localPath"])})
				} else {
					changes = append(changes, Change{Link: link, Op: "noop", What: whatM})
				}
			}
		}
		if !found {
			changes = append(changes, Change{Link: link, Op: "create", What: whatM, Apply: func(ctx context.Context) error {
				return a.do(ctx, "POST", "/remotepathmapping", obj{"host": host, "remotePath": remote, "localPath": local}, nil)
			}})
		}
	}
	return changes, nil
}
