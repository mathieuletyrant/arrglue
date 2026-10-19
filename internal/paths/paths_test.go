package paths

import (
	"strings"
	"testing"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

func TestCheck(t *testing.T) {
	Device = func(p string) (uint64, error) {
		if strings.HasPrefix(p, "/mnt/other") {
			return 2, nil
		}
		return 1, nil
	}
	tr := &graph.Instance{Name: "transmission", Mounts: []graph.Mount{{App: "/downloads", Host: "/srv/downloads"}}}
	radarr := &graph.Instance{Name: "radarr", Mounts: []graph.Mount{
		{App: "/downloads", Host: "/srv/downloads"},
		{App: "/movies", Host: "/srv/movies"},
		{App: "/data", Host: "/srv"},
		{App: "/ext", Host: "/mnt/other"},
	}}
	// Umbrel default: separate mounts, same disk
	got := levels(Check(radarr, tr, []string{"/movies"}, []string{"/downloads"}))
	if got != "OK WARN" {
		t.Errorf("separate mounts: %s", got)
	}
	// both apps mount the same parent: hardlinks ok
	trData := &graph.Instance{Name: "transmission", Mounts: []graph.Mount{{App: "/data", Host: "/srv"}}}
	one := &graph.Instance{Name: "radarr", Mounts: []graph.Mount{{App: "/data", Host: "/srv"}}}
	if got := levels(Check(one, trData, []string{"/data/movies"}, []string{"/data/downloads"})); got != "OK OK" {
		t.Errorf("common parent: %s", got)
	}
	// different filesystem
	if got := levels(Check(radarr, tr, []string{"/ext/movies"}, []string{"/downloads"})); got != "OK WARN" {
		t.Errorf("other fs: %s", got)
	}
	// radarr sees the download dir under another path -> remote path mapping
	sonarr := &graph.Instance{Name: "sonarr", Mounts: []graph.Mount{{App: "/data", Host: "/srv"}}}
	f := Check(sonarr, tr, []string{"/data/tv"}, []string{"/downloads"})
	if levels(f) != "WARN OK" || !strings.Contains(f[0].Msg, "as /data/downloads") {
		t.Errorf("remap: %+v", f)
	}
	// invisible
	blind := &graph.Instance{Name: "blind", Mounts: []graph.Mount{{App: "/movies", Host: "/elsewhere"}}}
	if got := levels(Check(blind, tr, []string{"/movies"}, []string{"/downloads"})); got != "ERROR" {
		t.Errorf("invisible: %s", got)
	}
}

func levels(f []Finding) string {
	var s []string
	for _, x := range f {
		s = append(s, x.Level)
	}
	return strings.Join(s, " ")
}
