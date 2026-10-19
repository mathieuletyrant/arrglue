// Package paths reads the directories apps actually use (via their APIs) and
// checks, per servarr -> download client pair, visibility and hardlink-ability.
package paths

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/mathieuletyrant/arrglue/internal/catalog"
	"github.com/mathieuletyrant/arrglue/internal/graph"
)

type Finding struct {
	Level string // OK | WARN | ERROR
	Msg   string
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
	res, err := hc.Do(req)
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

// Device returns the filesystem id of a host path; used to tell whether two
// paths can be hardlinked. Injected so tests run without the real tree.
var Device = func(p string) (uint64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Dev), nil
}

// Check compares the download client's dir with each root folder of the servarr app.
func Check(arr, client *graph.Instance, roots, downloadDirs []string) []Finding {
	var f []Finding
	for _, d := range downloadDirs {
		host, ok := client.ToHost(d)
		if !ok {
			f = append(f, Finding{"ERROR", fmt.Sprintf("%s download dir %s is not on a mount of %s: cannot map it to the host", client.Name, d, client.Name)})
			continue
		}
		seen, ok := arr.FromHost(host)
		if !ok {
			f = append(f, Finding{"ERROR", fmt.Sprintf("%s cannot see %s download dir %s (host %s): mount it into %s", arr.Name, client.Name, d, host, arr.Name)})
			continue
		}
		if seen != d {
			f = append(f, Finding{"WARN", fmt.Sprintf("%s sees %s download dir %s as %s: remote path mapping needed (plan will add it)", arr.Name, client.Name, d, seen)})
		} else {
			f = append(f, Finding{"OK", fmt.Sprintf("%s and %s both see %s (host %s)", arr.Name, client.Name, d, host)})
		}
		dlDev, dlErr := Device(host)
		for _, r := range roots {
			rHost, ok := arr.ToHost(r)
			if !ok {
				f = append(f, Finding{"ERROR", fmt.Sprintf("%s root folder %s is not on a mount of %s", arr.Name, r, arr.Name)})
				continue
			}
			rDev, rErr := Device(rHost)
			switch {
			case dlErr != nil || rErr != nil:
				f = append(f, Finding{"WARN", fmt.Sprintf("%s root %s: cannot check hardlinks, host paths not readable from here (%s, %s)", arr.Name, r, host, rHost)})
			case dlDev != rDev:
				f = append(f, Finding{"WARN", fmt.Sprintf("%s root %s (host %s) and downloads (host %s) are on different filesystems: no hardlinks, imports will copy", arr.Name, r, rHost, host)})
			case arr.MountFor(seen) != arr.MountFor(r):
				f = append(f, Finding{"WARN", fmt.Sprintf("%s root %s and downloads %s are separate mounts of the same disk: no hardlinks inside the container, mount a common parent (e.g. /data)", arr.Name, r, seen)})
			default:
				f = append(f, Finding{"OK", fmt.Sprintf("%s root %s and downloads %s: hardlinks possible", arr.Name, r, seen)})
			}
		}
	}
	return f
}
