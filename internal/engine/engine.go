// Package engine runs the plan over every linked pair and applies changes.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/connector"
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/reach"
)

// Plan computes changes for every catalog link that has a connector.
func Plan(ctx context.Context, env *connector.Env, cat *catalog.Catalog, insts []graph.Instance) ([]connector.Change, error) {
	var out []connector.Change
	for _, p := range reach.Pairs(cat, insts) {
		from, to := p[0], p[1]
		for _, c := range connector.All {
			if !c.Matches(from, to) {
				continue
			}
			if env.URL(nil, from) == "" {
				out = append(out, connector.Change{Link: from.Name + " -> " + to.Name, Op: "skip", What: "link", Detail: "arrglue cannot reach " + from.Name + "'s API (see doctor)"})
				continue
			}
			t, skip := c.Target(env, from, to)
			if t == nil {
				out = append(out, connector.Change{Link: from.Name + " -> " + to.Name, Op: "skip", What: "link", Detail: skip})
				continue
			}
			ch, err := t.Plan(ctx, env.Adopt)
			if err != nil {
				return out, fmt.Errorf("%s -> %s: %w", from.Name, to.Name, err)
			}
			out = append(out, ch...)
		}
	}
	return out, nil
}

// Test asks each caller's API to test its existing link objects (URL + key).
// Returns one Finding-like line per pair: level OK | WARN (not configured) | ERROR.
type TestResult struct{ Level, Link, What, Msg string }

func Test(ctx context.Context, env *connector.Env, cat *catalog.Catalog, insts []graph.Instance) []TestResult {
	var out []TestResult
	for _, p := range reach.Pairs(cat, insts) {
		from, to := p[0], p[1]
		for _, c := range connector.All {
			if !c.Matches(from, to) || env.URL(nil, from) == "" {
				continue
			}
			t, skip := c.Target(env, from, to)
			link := from.Name + " -> " + to.Name
			if t == nil {
				out = append(out, TestResult{"WARN", link, "-", skip})
				continue
			}
			found, err := t.Test(ctx)
			var pending connector.Pending
			switch {
			case errors.As(err, &pending):
				out = append(out, TestResult{"WARN", link, "-", string(pending)})
			case err != nil && found:
				out = append(out, TestResult{"ERROR", link, t.What(), "fails its own test: " + err.Error()})
			case err != nil:
				out = append(out, TestResult{"ERROR", link, t.What(), err.Error()})
			case !found:
				out = append(out, TestResult{"WARN", link, "-", "not configured yet (run plan to see why or fix it)"})
			default:
				out = append(out, TestResult{"OK", link, t.What(), "tested by " + from.Name + ": connection and key OK"})
			}
		}
	}
	return out
}
