// Package engine runs the plan over every linked pair and applies changes.
package engine

import (
	"context"
	"fmt"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/connector"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

// Plan computes changes for every catalog link that has a connector.
func Plan(ctx context.Context, env *connector.Env, cat *catalog.Catalog, insts []graph.Instance) ([]connector.Change, error) {
	var out []connector.Change
	for i := range insts {
		from := &insts[i]
		for _, kind := range cat.Apps[from.Kind].Links {
			for j := range insts {
				to := &insts[j]
				if to.Kind != kind {
					continue
				}
				for _, c := range connector.All {
					if !c.Matches(from, to) {
						continue
					}
					ch, err := c.Plan(ctx, env, from, to)
					if err != nil {
						return out, fmt.Errorf("%s -> %s: %w", from.Name, to.Name, err)
					}
					out = append(out, ch...)
				}
			}
		}
	}
	return out, nil
}
