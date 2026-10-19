// Package paths reads the directories apps actually use (via their APIs) and
// checks, per servarr -> download client pair, visibility and hardlink-ability.
package paths

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

type Finding struct {
	Level string // OK | WARN | ERROR
	Scope string // "radarr_server_1 <- transmission_server_1" or just the app
	Path  string // the path(s) concerned, as the servarr app sees them
	Msg   string // short verdict
}

// Dirs returns the directories an app uses, as the app sees them:
// root folders for servarr apps, the download dir for transmission.
func Dirs(ctx context.Context, in *graph.Instance, app *catalog.App, base string) ([]string, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	switch {
	case app.API.RootFolders != "":
		req, _ := http.NewRequestWithContext(ctx, "GET", base+app.API.RootFolders, nil)
		req.Header.Set("X-Api-Key", in.Secret.Value)
		var rows []struct {
			Path string `json:"path"`
		}
		if err := do(hc, req, &rows); err != nil {
			return nil, err
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Path)
		}
		return out, nil
	case in.Kind == "transmission":
		if in.Config["auth_required"] == "true" {
			return nil, fmt.Errorf("rpc auth required and no credentials known") // ponytail: manual creds later
		}
		body := []byte(`{"method":"session-get","arguments":{"fields":["download-dir"]}}`)
		req, _ := http.NewRequestWithContext(ctx, "POST", base+"/rpc", bytes.NewReader(body))
		var res struct {
			Arguments struct {
				Dir string `json:"download-dir"`
			} `json:"arguments"`
		}
		err := do(hc, req, &res)
		if sid := req.Response; err != nil && sid != nil && sid.StatusCode == 409 { // retry with session id
			req, _ = http.NewRequestWithContext(ctx, "POST", base+"/rpc", bytes.NewReader(body))
			req.Header.Set("X-Transmission-Session-Id", sid.Header.Get("X-Transmission-Session-Id"))
			err = do(hc, req, &res)
		}
		if err != nil {
			return nil, err
		}
		return []string{res.Arguments.Dir}, nil
	}
	return nil, nil
}

func do(hc *http.Client, req *http.Request, out any) error {
	start := time.Now()
	res, err := hc.Do(req)
	if os.Getenv("ARRGLUE_DEBUG") != "" {
		log.Printf("%6dms %s %s", time.Since(start).Milliseconds(), req.Method, req.URL)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	req.Response = res
	if res.StatusCode != 200 {
		return fmt.Errorf("%s %s: %s", req.Method, req.URL, res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

// Device returns the filesystem id of a host path (or of its nearest existing
// parent: the dir may not be created yet, the filesystem is what matters).
// Injected so tests run without the real tree.
var Device = func(p string) (uint64, error) {
	for {
		fi, err := os.Stat(p)
		if err == nil {
			return uint64(fi.Sys().(*syscall.Stat_t).Dev), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, err
		}
		p = parent
	}
}

// Check compares the download client's dir with each root folder of the servarr app.
func Check(arr, client *graph.Instance, roots, downloadDirs []string) []Finding {
	var f []Finding
	pair := arr.Name + " <- " + client.Name
	for _, d := range downloadDirs {
		host, ok := client.ToHost(d)
		if !ok {
			f = append(f, Finding{"ERROR", pair, d, "download dir is not on a mount of " + client.Name + ": cannot map it"})
			continue
		}
		seen, ok := arr.FromHost(host)
		if !ok {
			f = append(f, Finding{"ERROR", pair, d, fmt.Sprintf("download dir (host %s) is not visible to %s: mount it", host, arr.Name)})
			continue
		}
		if seen != d {
			f = append(f, Finding{"WARN", pair, d + " = " + seen, "same dir, different paths: remote path mapping needed (plan adds it)"})
		} else {
			f = append(f, Finding{"OK", pair, d, "download dir visible under the same path"})
		}
		dlDev, dlErr := Device(host)
		for _, r := range roots {
			rHost, ok := arr.ToHost(r)
			if !ok {
				f = append(f, Finding{"ERROR", arr.Name, r, "root folder is not on a mount of " + arr.Name})
				continue
			}
			rDev, rErr := Device(rHost)
			where := r + " <- " + seen
			switch {
			case dlErr != nil || rErr != nil:
				f = append(f, Finding{"WARN", arr.Name, where, "cannot check hardlinks: host paths not readable from here"})
			case dlDev != rDev:
				f = append(f, Finding{"WARN", arr.Name, where, "different filesystems: no hardlinks, imports will copy"})
			case arr.MountFor(seen) != arr.MountFor(r):
				f = append(f, Finding{"WARN", arr.Name, where, "separate mounts of the same disk: no hardlinks in the container, mount a common parent (e.g. /data)"})
			default:
				f = append(f, Finding{"OK", arr.Name, where, "hardlinks possible"})
			}
		}
	}
	return f
}
