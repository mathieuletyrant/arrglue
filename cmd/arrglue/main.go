// arrglue: discover *arr apps and wire them together.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/connector"
	"github.com/mathieuletyrant/arrglue/internal/engine"
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/paths"
	"github.com/mathieuletyrant/arrglue/internal/provider/docker"
	"github.com/mathieuletyrant/arrglue/internal/reach"
)

func main() {
	catDir := flag.String("catalog", os.Getenv("ARRGLUE_CATALOG"), "dir of YAML files overriding the embedded catalog")
	asJSON := flag.Bool("json", false, "machine-readable output")
	yes := flag.Bool("yes", false, "apply without asking")
	adopt := flag.Bool("adopt", false, "take over equivalent objects created by hand (tags them arrglue)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: arrglue [-catalog dir] [-json] [-yes] [-adopt] scan|doctor|plan|apply")
		flag.PrintDefaults()
	}
	flag.Parse()

	cat, err := catalog.Load(*catDir)
	fail(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli := docker.New()
	insts, err := cli.Discover(ctx, cat)
	fail(err)

	self := cli.Self(ctx)
	// urls: how arrglue itself reaches each instance (used to read their APIs)
	urls := map[string]string{}
	for i := range insts {
		in := &insts[i]
		url, v, err := reach.Self(ctx, self, in, cat.Apps[in.Kind])
		in.Version, urls[in.ID] = v, url
		if err != nil {
			in.Errors = append(in.Errors, "health: "+err.Error())
		}
	}

	dirs := map[string][]string{}
	if cmd := flag.Arg(0); cmd == "doctor" || cmd == "plan" || cmd == "apply" {
		for i := range insts {
			in := &insts[i]
			if urls[in.ID] == "" {
				continue
			}
			d, err := paths.Dirs(ctx, in, cat.Apps[in.Kind], urls[in.ID])
			if err != nil {
				in.Errors = append(in.Errors, "cannot read directories: "+err.Error())
			}
			dirs[in.ID] = d
		}
	}

	env := &connector.Env{Cat: cat, Dirs: dirs, Adopt: *adopt, URL: func(from, to *graph.Instance) string {
		if from == nil {
			return urls[to.ID]
		}
		return reach.Check(ctx, self, from, to, cat.Apps[to.Kind]).URL
	}}

	switch flag.Arg(0) {
	case "plan", "apply":
		changes, err := engine.Plan(ctx, env, cat, insts)
		fail(err)
		todo := 0
		for _, c := range changes {
			if c.Apply != nil {
				todo++
			}
			fmt.Printf("%-6s %s: %s\n", c.Op, c.Link, c.What)
			if c.Detail != "" {
				fmt.Println(c.Detail)
			}
		}
		fmt.Printf("\n%d change(s) to apply.\n", todo)
		if flag.Arg(0) == "plan" || todo == 0 {
			return
		}
		if !*yes {
			fmt.Print("Apply? [y/N] ")
			var ans string
			fmt.Scanln(&ans)
			if ans != "y" && ans != "Y" {
				return
			}
		}
		for _, c := range changes {
			if c.Apply == nil {
				continue
			}
			if err := c.Apply(ctx); err != nil {
				fmt.Printf("FAILED %s: %s: %v\n", c.Link, c.What, err)
				os.Exit(1)
			}
			fmt.Printf("done   %s: %s\n", c.Link, c.What)
		}
	case "scan":
		if *asJSON {
			json.NewEncoder(os.Stdout).Encode(insts)
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tVERSION\tNETWORKS\tPORTS\tKEY\tCONFIG")
		for _, in := range insts {
			key := "-"
			if in.Secret.Value != "" {
				key = "found"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", in.ID, in.Kind, or(in.Version, "?"),
				strings.Join(in.Networks, ","), ports(in), key, strings.Join(in.Errors, "; "))
		}
		w.Flush()
	case "doctor":
		fmt.Printf("arrglue runs as %s on %s\n", self.Name, strings.Join(self.Networks, ","))
		d := &doc{}
		for i := range insts {
			in := &insts[i]
			key := "key -"
			if _, wants := cat.Apps[in.Kind].Config.Fields["api_key"]; wants {
				key = "key ✓"
				if in.Secret.Value == "" {
					key = "key ✗"
				}
			}
			lvl, msg := "OK", urls[in.ID]
			if len(in.Errors) > 0 || key == "key ✗" {
				lvl, msg = "ERROR", strings.Join(in.Errors, "; ")
				if key == "key ✗" {
					msg = strings.TrimPrefix("no api key found; "+msg, "; ")
				}
			}
			d.add("APPS", lvl, in.Name, in.Kind, or(in.Version, "-"), key, msg)
		}
		pairs := reach.Pairs(cat, insts)
		for _, p := range pairs {
			from, to := p[0], p[1]
			r := reach.Check(ctx, self, from, to, cat.Apps[to.Kind])
			arrow := from.Name + " → " + to.Name
			switch {
			case r.URL == "":
				d.add("ROUTES", "ERROR", arrow, "-", r.Err)
			case r.Verified:
				d.add("ROUTES", "OK", arrow, r.URL, "")
			case strings.HasPrefix(r.Err, "unverified"):
				d.add("ROUTES", "WARN", arrow, r.URL, r.Err)
			default:
				d.add("ROUTES", "ERROR", arrow, r.URL, r.Err)
			}
		}
		for i := range insts {
			if arr := &insts[i]; cat.Apps[arr.Kind].API.RootFolders != "" && urls[arr.ID] != "" && len(dirs[arr.ID]) == 0 {
				d.add("PATHS", "WARN", arr.Name, "-", "no root folder configured yet")
			}
		}
		for _, p := range pairs {
			arr, client := p[0], p[1]
			if cat.Apps[arr.Kind].API.RootFolders == "" || cat.Apps[client.Kind].Family != "downloader" {
				continue
			}
			for _, f := range paths.Check(arr, client, dirs[arr.ID], dirs[client.ID]) {
				d.add("PATHS", f.Level, strings.ReplaceAll(f.Scope, "<-", "←"), strings.ReplaceAll(f.Path, "<-", "←"), f.Msg)
			}
		}
		for _, r := range engine.Test(ctx, env, cat, insts) {
			d.add("LINKS", r.Level, strings.ReplaceAll(r.Link, "->", "→"), r.What, r.Msg)
		}
		d.print(map[string]string{
			"APPS":   "discovered apps, as reached by arrglue",
			"ROUTES": "which app can call which, and with what URL",
			"PATHS":  "download dirs vs library folders, hardlinks",
			"LINKS":  "what the apps have configured, tested by the apps themselves",
		})
		if d.errors > 0 {
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func ports(in graph.Instance) string {
	var s []string
	for _, p := range in.Ports {
		if p.Published != 0 {
			s = append(s, fmt.Sprintf("%d->%d", p.Published, p.Internal))
		} else {
			s = append(s, fmt.Sprint(p.Internal))
		}
	}
	return or(strings.Join(s, ","), "-")
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "arrglue:", err)
		os.Exit(1)
	}
}

// doc collects doctor findings per section and prints them aligned.
type doc struct {
	order         []string
	rows          map[string][][]string
	errors, warns int
}

func (d *doc) add(section, level string, cols ...string) {
	if d.rows == nil {
		d.rows = map[string][][]string{}
	}
	if _, ok := d.rows[section]; !ok {
		d.order = append(d.order, section)
	}
	sym := map[string]string{"OK": "✓", "WARN": "!", "ERROR": "✗"}[level]
	switch level {
	case "ERROR":
		d.errors++
	case "WARN":
		d.warns++
	}
	d.rows[section] = append(d.rows[section], append([]string{sym}, cols...))
}

func (d *doc) print(titles map[string]string) {
	for _, sec := range d.order {
		fmt.Printf("\n%s  %s\n", sec, titles[sec])
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, r := range d.rows[sec] {
			fmt.Fprintln(w, "  "+strings.Join(r, "\t"))
		}
		w.Flush()
	}
	fmt.Printf("\n%d error(s), %d warning(s)\n", d.errors, d.warns)
}
