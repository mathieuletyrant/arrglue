// arrglue: discover *arr apps and wire them together.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/connector"
	"github.com/mathieuletyrant/arrglue/internal/engine"
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/paths"
	"github.com/mathieuletyrant/arrglue/internal/provider/docker"
	"github.com/mathieuletyrant/arrglue/internal/reach"
)

var catDir, secretsFile string

func main() {
	root := &cobra.Command{Use: "arrglue", Short: "Discover the *arr stack and wire the apps together", SilenceUsage: true}
	root.PersistentFlags().StringVar(&catDir, "catalog", os.Getenv("ARRGLUE_CATALOG"), "dir of YAML files overriding the embedded catalog")
	root.PersistentFlags().StringVar(&secretsFile, "secrets", os.Getenv("ARRGLUE_SECRETS"), "YAML file of api keys arrglue cannot read itself: {<container name>: <key>}")

	var asJSON bool
	scan := &cobra.Command{Use: "scan", Short: "List the discovered apps (writes nothing)", RunE: func(cmd *cobra.Command, _ []string) error {
		st, err := load(cmd.Context(), false)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(st.insts)
		}
		printScan(st.insts)
		return nil
	}}
	scan.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")

	doctor := &cobra.Command{Use: "doctor", Short: "Check apps, routes, paths and configured links (writes nothing)", RunE: func(cmd *cobra.Command, _ []string) error {
		st, err := load(cmd.Context(), true)
		if err != nil {
			return err
		}
		rep := report(cmd.Context(), st)
		printDoctor(rep)
		if rep.Errors > 0 {
			os.Exit(1)
		}
		return nil
	}}

	var adopt, yes bool
	var only string
	filter := func(changes []connector.Change) []connector.Change {
		if only == "" {
			return changes
		}
		var out []connector.Change
		for _, c := range changes {
			if strings.Contains(c.Link, only) {
				out = append(out, c)
			}
		}
		return out
	}
	plan := &cobra.Command{Use: "plan", Short: "Show what apply would change, Terraform-style (writes nothing)", RunE: func(cmd *cobra.Command, _ []string) error {
		st, err := load(cmd.Context(), true)
		if err != nil {
			return err
		}
		st.env.Adopt = adopt
		changes, err := engine.Plan(cmd.Context(), st.env, st.cat, st.insts)
		if err != nil {
			return err
		}
		printPlan(filter(changes))
		return nil
	}}
	apply := &cobra.Command{Use: "apply", Short: "Apply the plan through the apps' APIs, after confirmation", RunE: func(cmd *cobra.Command, _ []string) error {
		st, err := load(cmd.Context(), true)
		if err != nil {
			return err
		}
		st.env.Adopt = adopt
		changes, err := engine.Plan(cmd.Context(), st.env, st.cat, st.insts)
		if err != nil {
			return err
		}
		changes = filter(changes)
		if printPlan(changes) == 0 {
			return nil
		}
		if !yes {
			fmt.Print("\nApply? [y/N] ")
			var ans string
			fmt.Scanln(&ans)
			if ans != "y" && ans != "Y" {
				return nil
			}
		}
		fmt.Println()
		failed := 0
		for _, c := range changes {
			if c.Apply == nil {
				continue
			}
			if err := c.Apply(cmd.Context()); err != nil {
				failed++
				fmt.Printf("  %s %s  %s: %v\n", sym("ERROR"), arrow(c.Link), c.What, err)
				continue
			}
			fmt.Printf("  %s %s  %s\n", sym("OK"), arrow(c.Link), c.What)
		}
		if failed > 0 {
			return fmt.Errorf("%d change(s) failed", failed)
		}
		return nil
	}}
	for _, c := range []*cobra.Command{plan, apply} {
		c.Flags().BoolVar(&adopt, "adopt", false, "take over equivalent objects created by hand (tags them arrglue)")
		c.Flags().StringVar(&only, "only", "", "limit to links whose names contain this text, e.g. jellyfin or prowlarr->radarr")
	}
	apply.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	root.AddCommand(scan, doctor, plan, apply, serveCmd())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "arrglue:", err)
		os.Exit(1)
	}
}

// state is everything the commands share: catalog, instances, how arrglue
// reaches each of them, and the directories they use.
type state struct {
	cat   *catalog.Catalog
	insts []graph.Instance
	self  *graph.Instance
	urls  map[string]string   // instance ID -> base URL from arrglue's vantage
	dirs  map[string][]string // instance ID -> dirs it uses
	env   *connector.Env
}

func load(ctx context.Context, withDirs bool) (*state, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cat, err := catalog.Load(catDir)
	if err != nil {
		return nil, err
	}
	cli := docker.New()
	insts, err := cli.Discover(ctx, cat)
	if err != nil {
		return nil, err
	}
	if secretsFile != "" {
		b, err := os.ReadFile(secretsFile)
		if err != nil {
			return nil, err
		}
		manual := map[string]string{}
		if err := yaml.Unmarshal(b, &manual); err != nil {
			return nil, fmt.Errorf("%s: %w", secretsFile, err)
		}
		for i := range insts {
			if k := manual[insts[i].Name]; k != "" {
				insts[i].Secret = graph.SecretRef{Source: "file:" + secretsFile, Value: k}
			}
		}
	}
	st := &state{cat: cat, insts: insts, self: cli.Self(ctx), urls: map[string]string{}, dirs: map[string][]string{}}
	reach.ResetCache()
	type probed struct {
		url  string
		dirs []string
	}
	results := reach.Parallel(len(insts), func(i int) probed {
		in := &insts[i]
		url, v, err := reach.Self(ctx, st.self, in, cat.Apps[in.Kind])
		in.Version = v
		if err != nil {
			in.Errors = append(in.Errors, "health: "+err.Error())
		}
		var dirs []string
		if withDirs && url != "" {
			if dirs, err = paths.Dirs(ctx, in, cat.Apps[in.Kind], url); err != nil {
				in.Errors = append(in.Errors, "cannot read directories: "+err.Error())
			}
		}
		return probed{url, dirs}
	})
	for i, r := range results {
		st.urls[insts[i].ID], st.dirs[insts[i].ID] = r.url, r.dirs
	}
	st.env = &connector.Env{Cat: cat, Dirs: st.dirs, URL: func(from, to *graph.Instance) string {
		if from == nil {
			return st.urls[to.ID]
		}
		return reach.Check(context.WithoutCancel(ctx), st.self, from, to, cat.Apps[to.Kind]).URL
	}}
	return st, nil
}
