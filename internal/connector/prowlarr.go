package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// prowlarr -> radarr/sonarr: an "application" in Prowlarr, which needs two
// URLs: the app as seen by Prowlarr, and Prowlarr as seen by the app.
type prowlarr struct{}

func (prowlarr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "prowlarr" && (to.Kind == "radarr" || to.Kind == "sonarr")
}

func (prowlarr) Plan(ctx context.Context, env *Env, from, to *graph.Instance) ([]Change, error) {
	link := from.Name + " -> " + to.Name
	what := fmt.Sprintf("application %s (%s) in %s", title(to.Kind), to.Name, from.Name)
	appURL, backURL := env.URL(from, to), env.URL(to, from)
	if appURL == "" || backURL == "" {
		return []Change{{Link: link, Op: "skip", What: what, Detail: "no route between the two apps (see doctor)"}}, nil
	}
	if to.Secret.Value == "" {
		return []Change{{Link: link, Op: "skip", What: what, Detail: "no api key known for " + to.Name}}, nil
	}
	a := newAPI(env.URL(nil, from), env.Cat.Apps[from.Kind], from.Secret.Value)
	identity := func(o obj) bool { return sameEndpoint(fmt.Sprint(o.field("baseUrl")), appURL) }
	fill := func(o obj, force bool) []string {
		var d []string
		if o["name"] == nil || o["name"] == "" { // keep hand-given names on adopt
			d = append(d, "  name: "+to.Name)
			o["name"] = to.Name
		}
		if o["syncLevel"] != "fullSync" {
			d = append(d, fmt.Sprintf("  syncLevel: %v -> fullSync", o["syncLevel"]))
			o["syncLevel"] = "fullSync"
		}
		for k, v := range map[string]string{"baseUrl": appURL, "prowlarrUrl": backURL, "apiKey": to.Secret.Value} {
			if l := o.set(k, v, force); l != "" {
				d = append(d, l)
			}
		}
		return d
	}
	return upsert(ctx, a, env.Adopt, "/applications", title(to.Kind), link, what, identity, fill)
}

func sameEndpoint(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}
