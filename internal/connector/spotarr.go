package connector

import (
	"context"
	"fmt"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// spotarr -> seerr / jellyfin: the two server slots of Spotarr's settings
// (Connect-RPC, bearer token = its API_TOKEN).
type spotarr struct{}

func (spotarr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "spotarr" && (to.Kind == "seerr" || to.Kind == "jellyfin")
}

func (spotarr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	if from.Secret.Value == "" {
		return nil, "Spotarr API token unknown (API_TOKEN env or --secrets)"
	}
	a := jsonAPI(env.URL(nil, from), "/api/spotarr.v1.SpotarrService", "Authorization", "Bearer "+from.Secret.Value)
	return &spotarrSlot{a: a, link: link, section: to.Kind, what: to.Kind + " server", want: map[string]any{"url": toURL, "apiKey": to.Secret.Value}}, ""
}

type spotarrSlot struct {
	a                   api
	link, section, what string
	want                map[string]any
}

func (t *spotarrSlot) What() string { return t.what }

// current returns the section's settings; nil when this Spotarr version has no such section.
func (t *spotarrSlot) current(ctx context.Context) (obj, error) {
	var res struct{ Settings map[string]obj }
	if err := t.a.do(ctx, "POST", "/GetSettings", obj{}, &res); err != nil {
		return nil, err
	}
	return res.Settings[t.section], nil
}

const spotarrNoSection = "this Spotarr version has no such settings section"

// Test compares what Spotarr stores with what it should hold: its own
// CheckDependencies also calls the LLM, TMDB, yt-dlp and ffmpeg over the
// internet, far too slow for a doctor run.
func (t *spotarrSlot) Test(ctx context.Context) (bool, error) {
	cur, err := t.current(ctx)
	if err != nil {
		return false, err
	}
	if cur == nil {
		return false, Pending(spotarrNoSection)
	}
	if cur["url"] == nil || cur["url"] == "" {
		return false, nil
	}
	for k, v := range t.want {
		if fmt.Sprint(cur[k]) != fmt.Sprint(v) {
			if k == "apiKey" {
				return true, fmt.Errorf("stored api key differs from %s's current key (run plan)", t.section)
			}
			return true, fmt.Errorf("stored %s is %v, expected %v (run plan)", k, cur[k], v)
		}
	}
	return true, nil
}

func (t *spotarrSlot) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	cur, err := t.current(ctx)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return []Change{{Link: t.link, Op: "skip", What: t.what, Detail: spotarrNoSection}}, nil
	}
	return []Change{slot(t.link, t.what, cur, t.want, adopt, func(ctx context.Context) error {
		return t.a.do(ctx, "POST", "/UpdateSettings", obj{"settings": obj{t.section: t.want}}, nil)
	})}, nil
}
