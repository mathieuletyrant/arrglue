package connector

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// radarr/sonarr -> jellyfin: an "Emby / Jellyfin" notification that refreshes
// the library on import, upgrade, rename and delete.
type mediaNotify struct{}

func (mediaNotify) Matches(from, to *graph.Instance) bool {
	return (from.Kind == "radarr" || from.Kind == "sonarr") && to.Kind == "jellyfin"
}

func (mediaNotify) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	what := fmt.Sprintf("library refresh notification %q", to.Name)
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, to.Name + " has no API key yet: create one in Jellyfin (Dashboard → API Keys), arrglue picks it up"
	}
	u, _ := url.Parse(toURL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	a := newAPI(env.URL(nil, from), env.Cat.Apps[from.Kind], from.Secret.Value)
	identity := func(o obj) bool {
		return fmt.Sprint(o.field("host")) == host && fmt.Sprint(o.field("port")) == fmt.Sprint(port)
	}
	fill := func(o obj, force bool) []string {
		var d []string
		if o["name"] == nil || o["name"] == "" {
			d = append(d, "  name: "+to.Name)
			o["name"] = to.Name
		}
		for k, v := range map[string]any{"host": host, "port": port, "useSsl": false, "urlBase": u.Path, "apiKey": to.Secret.Value, "updateLibrary": true} {
			if l := o.set(k, v, force); l != "" {
				d = append(d, l)
			}
		}
		// fire on every library change the app supports
		for _, ev := range []string{"onDownload", "onUpgrade", "onRename", "onMovieDelete", "onMovieFileDelete", "onSeriesDelete", "onEpisodeFileDelete"} {
			if o["supports"+strings.ToUpper(ev[:1])+ev[1:]] == true && o[ev] != true {
				d = append(d, "  "+ev+": true")
				o[ev] = true
			}
		}
		return d
	}
	return &servarrTarget{API: a, Path: "/notification", Implementation: "MediaBrowser", Link: link, Desc: what, Identity: identity, Fill: fill}, ""
}
