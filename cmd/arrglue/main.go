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
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/paths"
	"github.com/mathieuletyrant/arrglue/internal/provider/docker"
	"github.com/mathieuletyrant/arrglue/internal/reach"
)

func main() {
	catDir := flag.String("catalog", os.Getenv("ARRGLUE_CATALOG"), "dir of YAML files overriding the embedded catalog")
	asJSON := flag.Bool("json", false, "machine-readable output")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: arrglue [-catalog dir] [-json] scan|doctor")
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

	switch flag.Arg(0) {
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
		bad := 0
		report := func(level, scope, msg string) {
			if level == "ERROR" {
				bad++
			}
			fmt.Printf("%-5s %s: %s\n", level, scope, msg)
		}
		fmt.Printf("arrglue runs as %s on networks %v\n\n", self.Name, self.Networks)
		byKind := map[string][]*graph.Instance{}
		for i := range insts {
			in := &insts[i]
			byKind[in.Kind] = append(byKind[in.Kind], in)
			for _, e := range in.Errors {
				report("ERROR", in.ID, e)
			}
			if _, wantsKey := cat.Apps[in.Kind].Config.Fields["api_key"]; wantsKey && in.Secret.Value == "" {
				report("ERROR", in.ID, "no api key found")
			}
			if urls[in.ID] != "" {
				report("OK", in.ID, in.Kind+" "+or(in.Version, "reachable")+" at "+urls[in.ID])
			}
		}
		fmt.Println()
		// reachability per declared pair (catalog links)
		dirs := map[string][]string{}
		for i := range insts {
			from := &insts[i]
			for _, kind := range cat.Apps[from.Kind].Links {
				for _, to := range byKind[kind] {
					r := reach.Check(ctx, self, from, to, cat.Apps[to.Kind])
					switch {
					case r.URL == "":
						report("ERROR", from.Name+" -> "+to.Name, r.Err)
					case r.Verified:
						report("OK", from.Name+" -> "+to.Name, r.URL)
					case strings.HasPrefix(r.Err, "unverified"):
						report("WARN", from.Name+" -> "+to.Name, r.URL+" ("+r.Err+")")
					default:
						report("ERROR", from.Name+" -> "+to.Name, r.URL+": "+r.Err)
					}
				}
			}
		}
		fmt.Println()
		// paths: servarr root folders vs download client dirs
		for i := range insts {
			in := &insts[i]
			if urls[in.ID] == "" {
				continue
			}
			d, err := paths.Dirs(ctx, in, cat.Apps[in.Kind], urls[in.ID])
			if err != nil {
				report("WARN", in.ID, "cannot read directories: "+err.Error())
			}
			dirs[in.ID] = d
		}
		for i := range insts {
			arr := &insts[i]
			if cat.Apps[arr.Kind].API.RootFolders == "" {
				continue
			}
			if len(dirs[arr.ID]) == 0 {
				report("WARN", arr.ID, "no root folder configured yet")
			}
			for _, kind := range cat.Apps[arr.Kind].Links {
				for _, client := range byKind[kind] {
					if cat.Apps[kind].Family != "downloader" {
						continue
					}
					for _, f := range paths.Check(arr, client, dirs[arr.ID], dirs[client.ID]) {
						report(f.Level, "paths", f.Msg)
					}
				}
			}
		}
		if bad > 0 {
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
