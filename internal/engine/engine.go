// Package engine runs the plan over every linked pair and applies changes.
package engine

import (
	"context"
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
				out = append(out, connector.Change{Link: from.Name + " -> " + to.Name, Op: "skip", What: "link", Detail: "arrglue cannot reach " + from.Name + " to read or write its API (see doctor)"})
				continue
			}
			ch, err := c.Plan(ctx, env, from, to)
			if err != nil {
				return out, fmt.Errorf("%s -> %s: %w", from.Name, to.Name, err)
			}
			out = append(out, ch...)
		}
	}
	return out, nil
}
