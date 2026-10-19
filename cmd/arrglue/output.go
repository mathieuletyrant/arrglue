package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/fatih/color"

	"github.com/mathieuletyrant/arrglue/internal/connector"
	"github.com/mathieuletyrant/arrglue/internal/engine"
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/paths"
	"github.com/mathieuletyrant/arrglue/internal/reach"
)

var (
	green  = color.New(color.FgGreen).SprintFunc()
	yellow = color.New(color.FgYellow).SprintFunc()
	red    = color.New(color.FgRed).SprintFunc()
	bold   = color.New(color.Bold).SprintFunc()
)

// sym renders a status or a plan operation as a colored symbol (plain when piped).
func sym(level string) string {
	switch level {
	case "OK", "noop":
		return green(map[string]string{"OK": "✓", "noop": "="}[level])
	case "WARN", "skip":
		return yellow(map[string]string{"WARN": "!", "skip": "-"}[level])
	case "ERROR":
		return red("✗")
	case "create":
		return green("+")
	case "update":
		return yellow("~")
	case "adopt":
		return yellow("±")
	case "manual":
		return yellow("✎")
	}
	return "?"
}

func arrow(link string) string {
	return strings.NewReplacer("->", "→", "<-", "←").Replace(link)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func table(rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(w, "  "+strings.Join(r, "\t"))
	}
	w.Flush()
}

func printScan(insts []graph.Instance) {
	rows := [][]string{{bold("NAME"), bold("KIND"), bold("VERSION"), bold("NETWORKS"), bold("PORTS"), bold("KEY"), bold("NOTES")}}
	for _, in := range insts {
		var ports []string
		for _, p := range in.Ports {
			if p.Published != 0 {
				ports = append(ports, fmt.Sprintf("%d→%d", p.Published, p.Internal))
			} else {
				ports = append(ports, fmt.Sprint(p.Internal))
			}
		}
		key := "-"
		if in.Secret.Value != "" {
			key = "✓"
		}
		rows = append(rows, []string{in.Name, in.Kind, or(in.Version, "-"), strings.Join(in.Networks, ","), or(strings.Join(ports, ","), "-"), key, strings.Join(in.Errors, "; ")})
	}
	table(rows)
}

// Report is the doctor's result: sections of leveled rows.
type Report struct {
	Self     string    `json:"self"`
	Sections []Section `json:"sections"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
}

type Section struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Rows  []Row  `json:"rows"`
}

type Row struct {
	Level string   `json:"level"` // OK | WARN | ERROR
	Cols  []string `json:"cols"`
}

// report runs the doctor checks: APPS / ROUTES / PATHS / LINKS.
func report(ctx context.Context, st *state) *Report {
	rep := &Report{Self: st.self.Name + " on " + strings.Join(st.self.Networks, ",")}
	rep.Sections = []Section{
		{Name: "APPS", Title: "discovered apps, as reached by arrglue"},
		{Name: "ROUTES", Title: "which app can call which, and with what URL"},
		{Name: "PATHS", Title: "download dirs vs library folders, hardlinks"},
		{Name: "LINKS", Title: "what the apps have configured, tested by the apps themselves"},
	}
	add := func(section int, level string, cols ...string) {
		switch level {
		case "ERROR":
			rep.Errors++
		case "WARN":
			rep.Warnings++
		}
		rep.Sections[section].Rows = append(rep.Sections[section].Rows, Row{level, cols})
	}
	for i := range st.insts {
		in := &st.insts[i]
		key := "key -"
		if _, wants := st.cat.Apps[in.Kind].Config.Fields["api_key"]; wants || st.cat.Apps[in.Kind].Secret.SQLite != "" {
			key = "key ✓"
			if in.Secret.Value == "" {
				key = "key ✗"
				in.Errors = append([]string{"no api key found"}, in.Errors...)
			}
		}
		if len(in.Errors) > 0 {
			add(0, "ERROR", in.Name, in.Kind, or(in.Version, "-"), key, strings.Join(in.Errors, "; "))
		} else {
			add(0, "OK", in.Name, in.Kind, or(in.Version, "-"), key, st.urls[in.ID])
		}
	}
	pairs := reach.Pairs(st.cat, st.insts)
	for _, p := range pairs {
		from, to := p[0], p[1]
		r := reach.Check(ctx, st.self, from, to, st.cat.Apps[to.Kind])
		link := from.Name + " → " + to.Name
		switch {
		case r.URL == "":
			add(1, "ERROR", link, "-", r.Err)
		case r.Verified:
			add(1, "OK", link, r.URL, "")
		case strings.HasPrefix(r.Err, "unverified"):
			add(1, "WARN", link, r.URL, r.Err)
		default:
			add(1, "ERROR", link, r.URL, r.Err)
		}
	}
	for i := range st.insts {
		if arr := &st.insts[i]; st.cat.Apps[arr.Kind].API.RootFolders != "" && st.urls[arr.ID] != "" && len(st.dirs[arr.ID]) == 0 {
			add(2, "WARN", arr.Name, "-", "no root folder configured yet")
		}
	}
	for _, p := range pairs {
		arr, client := p[0], p[1]
		if st.cat.Apps[arr.Kind].API.RootFolders == "" || st.cat.Apps[client.Kind].Family != "downloader" {
			continue
		}
		for _, f := range paths.Check(arr, client, st.dirs[arr.ID], st.dirs[client.ID]) {
			add(2, f.Level, arrow(f.Scope), arrow(f.Path), f.Msg)
		}
	}
	for _, r := range engine.Test(ctx, st.env, st.cat, st.insts) {
		add(3, r.Level, arrow(r.Link), r.What, r.Msg)
	}
	return rep
}

func printDoctor(rep *Report) {
	fmt.Printf("arrglue runs as %s\n", rep.Self)
	for _, s := range rep.Sections {
		if len(s.Rows) == 0 {
			continue
		}
		fmt.Printf("\n%s  %s\n", bold(s.Name), s.Title)
		var rows [][]string
		for _, r := range s.Rows {
			rows = append(rows, append([]string{sym(r.Level)}, r.Cols...))
		}
		table(rows)
	}
	fmt.Printf("\n%d error(s), %d warning(s)\n", rep.Errors, rep.Warnings)
}

// printPlan groups changes Terraform-style and returns how many will write.
func printPlan(changes []connector.Change) int {
	count := map[string]int{}
	var todo, skipped, same, manual [][]string
	for _, c := range changes {
		count[c.Op]++
		row := []string{sym(c.Op), arrow(c.Link), c.What, c.Detail}
		switch c.Op {
		case "manual":
			manual = append(manual, row)
		case "skip":
			skipped = append(skipped, row)
		case "noop":
			same = append(same, row)
		default:
			todo = append(todo, row)
		}
	}
	if len(todo) > 0 {
		fmt.Printf("%s  %s create  %s update  %s adopt\n", bold("CHANGES"), sym("create"), sym("update"), sym("adopt"))
		for _, r := range todo {
			fmt.Printf("  %s %s  %s\n", r[0], r[1], r[2])
			for _, l := range strings.Split(r[3], "\n") {
				if l = strings.TrimSpace(l); l != "" {
					fmt.Printf("      %s\n", arrow(l))
				}
			}
		}
	}
	if len(manual) > 0 {
		fmt.Printf("\n%s  config files arrglue cannot write: put this in yourself\n", bold("MANUAL"))
		for _, r := range manual {
			fmt.Printf("  %s %s  %s\n", r[0], r[1], r[2])
			for _, l := range strings.Split(r[3], "\n") {
				if l != "" {
					fmt.Printf("      %s\n", l)
				}
			}
		}
	}
	if len(skipped) > 0 {
		fmt.Printf("\n%s  left untouched (--adopt takes over hand-made objects)\n", bold("SKIPPED"))
		table(skipped)
	}
	if len(same) > 0 {
		fmt.Printf("\n%s  already as desired\n", bold("UNCHANGED"))
		table(same)
	}
	fmt.Printf("\nPlan: %d to create, %d to update, %d to adopt, %d manual, %d unchanged, %d skipped.\n",
		count["create"], count["update"], count["adopt"], count["manual"], count["noop"], count["skip"])
	return count["create"] + count["update"] + count["adopt"]
}
