package connector

import (
	"context"
	"fmt"

	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// lingarr -> radarr/sonarr: two slots in Lingarr's settings. Its api key is
// encrypted at rest (--secrets), unless its authentication is disabled.
type lingarr struct{}

func (lingarr) Matches(from, to *graph.Instance) bool {
	return from.Kind == "lingarr" && (to.Kind == "radarr" || to.Kind == "sonarr")
}

func (lingarr) Target(env *Env, from, to *graph.Instance) (Target, string) {
	link := from.Name + " -> " + to.Name
	toURL := env.URL(from, to)
	if toURL == "" {
		return nil, "no route between the two apps (see doctor)"
	}
	if to.Secret.Value == "" {
		return nil, "no api key known for " + to.Name
	}
	a := jsonAPI(env.URL(nil, from), "/api", "X-Api-Key", from.Secret.Value)
	return &lingarrSlot{a: a, link: link, kind: to.Kind, name: to.Name, hasKey: from.Secret.Value != "",
		want: map[string]any{to.Kind + "_url": toURL, to.Kind + "_api_key": to.Secret.Value}}, ""
}

type lingarrSlot struct {
	a                api
	link, kind, name string
	hasKey           bool
	want             map[string]any
}

func (t *lingarrSlot) What() string { return fmt.Sprintf("%s connection %q", t.kind, t.name) }

// ready checks Lingarr's onboarding and whether a key is needed to talk to it.
func (t *lingarrSlot) ready(ctx context.Context) error {
	var st struct {
		RequiresOnboarding bool   `json:"requiresOnboarding"`
		AuthType           string `json:"authType"`
	}
	if err := t.a.do(ctx, "GET", "/auth/authenticated", nil, &st); err != nil {
		return err
	}
	if st.RequiresOnboarding {
		return Pending("Lingarr onboarding not done yet (create its account once)")
	}
	if st.AuthType != "disabled" && !t.hasKey {
		return Pending("Lingarr api key unknown: copy it from Lingarr → Settings → Authentication into --secrets, or disable its authentication")
	}
	return nil
}

func (t *lingarrSlot) current(ctx context.Context) (obj, error) {
	var cur obj
	keys := []string{t.kind + "_url", t.kind + "_api_key"}
	if err := t.a.do(ctx, "POST", "/setting/multiple/get", keys, &cur); err != nil {
		return nil, err
	}
	return cur, nil
}

func (t *lingarrSlot) Test(ctx context.Context) (bool, error) {
	if err := t.ready(ctx); err != nil {
		return false, err
	}
	cur, err := t.current(ctx)
	if err != nil {
		return false, err
	}
	if cur[t.kind+"_url"] == nil || cur[t.kind+"_url"] == "" {
		return false, nil
	}
	for k, v := range t.want {
		if fmt.Sprint(cur[k]) != fmt.Sprint(v) {
			return true, fmt.Errorf("stored %s differs from the expected value (run plan)", k)
		}
	}
	return true, nil
}

func (t *lingarrSlot) Plan(ctx context.Context, adopt bool) ([]Change, error) {
	if err := t.ready(ctx); err != nil {
		if p, ok := err.(Pending); ok {
			return []Change{{Link: t.link, Op: "skip", What: t.What(), Detail: string(p)}}, nil
		}
		return nil, err
	}
	cur, err := t.current(ctx)
	if err != nil {
		return nil, err
	}
	return []Change{slot(t.link, t.What(), cur, t.want, adopt, func(ctx context.Context) error {
		body := map[string]string{t.kind + "_settings_completed": "true"}
		for k, v := range t.want {
			body[k] = fmt.Sprint(v)
		}
		return t.a.do(ctx, "POST", "/setting/multiple/set", body, nil)
	})}, nil
}
