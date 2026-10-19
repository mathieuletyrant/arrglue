// arrglue: discover *arr apps and wire them together.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
	"github.com/mathieuletyrant/arrglue/internal/provider/docker"
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
	insts, err := docker.New().Discover(ctx, cat)
	fail(err)

	switch flag.Arg(0) {
	case "scan":
		for i := range insts {
			insts[i].Version, _ = probe(ctx, &insts[i], cat.Apps[insts[i].Kind])
		}
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
		for i := range insts {
			in := &insts[i]
			app := cat.Apps[in.Kind]
			for _, e := range in.Errors {
				bad++
				fmt.Printf("ERROR %s: %s\n", in.ID, e)
			}
			if _, wantsKey := app.Config.Fields["api_key"]; wantsKey && in.Secret.Value == "" {
				bad++
				fmt.Printf("ERROR %s: no api key found\n", in.ID)
			}
			if v, err := probe(ctx, in, app); err != nil {
				bad++
				fmt.Printf("ERROR %s: health: %v\n", in.ID, err)
			} else {
				fmt.Printf("OK    %s: %s %s\n", in.ID, in.Kind, or(v, "reachable"))
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

// probe hits the catalog health endpoint from where arrglue runs: the published
// port on 127.0.0.1 if any, else the container name if it resolves (= we are on
// one of its Docker networks). Returns the version when the API reports one.
func probe(ctx context.Context, in *graph.Instance, app *catalog.App) (string, error) {
	if app.Health.Path == "" {
		return "", nil
	}
	port := in.HTTPPort(app.Ports["http"])
	host := ""
	switch {
	case in.PublishedPort(port) != 0:
		host = fmt.Sprintf("127.0.0.1:%d", in.PublishedPort(port))
	case len(in.Networks) > 0 && in.Networks[0] == "host":
		host = fmt.Sprintf("127.0.0.1:%d", port)
	default:
		if _, err := net.DefaultResolver.LookupHost(ctx, in.Name); err == nil {
			host = fmt.Sprintf("%s:%d", in.Name, port)
		}
	}
	if host == "" {
		return "", fmt.Errorf("unverified: no published port and %q does not resolve from here (run arrglue on its Docker network)", in.Name)
	}
	base := "/" + strings.Trim(in.Config["url_base"], "/")
	u := "http://" + host + strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(app.Health.Path, "/")
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	if app.Health.Auth.Header != "" && in.Secret.Value != "" {
		req.Header.Set(app.Health.Auth.Header, in.Secret.Value)
	}
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	ok := app.Health.OK
	if len(ok) == 0 {
		ok = []int{200}
	}
	accepted := false
	for _, s := range ok {
		accepted = accepted || s == res.StatusCode
	}
	if !accepted {
		return "", fmt.Errorf("GET %s: %s", u, res.Status)
	}
	if app.Health.VersionField == "" || res.StatusCode != 200 {
		return "", nil
	}
	var body map[string]any
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if json.Unmarshal(b, &body) == nil {
		return fmt.Sprint(body[app.Health.VersionField]), nil
	}
	return "", nil
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
