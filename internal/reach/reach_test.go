package reach

import (
	"strings"
	"testing"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

func TestCandidates(t *testing.T) {
	cat, _ := catalog.Load("")
	tr := &graph.Instance{Name: "transmission_server_1", Networks: []string{"umbrel_main_network"},
		Config: map[string]string{"port": "9091", "url_base": "/transmission/"}, Ports: []graph.Port{{Internal: 9091, Published: 19091}}}
	radarr := &graph.Instance{Name: "radarr_server_1", Networks: []string{"umbrel_main_network"}}
	bridge := &graph.Instance{Name: "other", Networks: []string{"bridge"}}
	host := &graph.Instance{Name: "native", Networks: []string{"host"}}
	for _, tc := range []struct {
		from *graph.Instance
		want string
	}{
		{radarr, "http://transmission_server_1:9091/transmission http://host.docker.internal:19091/transmission"},
		{bridge, "http://host.docker.internal:19091/transmission"},
		{host, "http://127.0.0.1:19091/transmission"},
	} {
		got := strings.Join(Candidates(tc.from, tr, cat.Apps["transmission"]), " ")
		if got != tc.want {
			t.Errorf("from %s: got %q want %q", tc.from.Name, got, tc.want)
		}
	}
	if c := Candidates(bridge, radarr, cat.Apps["radarr"]); len(c) != 0 {
		t.Errorf("bridge -> unpublished radarr should have no route, got %v", c)
	}
}
