# arrglue

Discovers the *arr stack wherever it runs, works out how each app can reach the
others, and wires them together through their APIs. Spec: [SPEC.md](SPEC.md).

Status: roadmap step 2 — catalog + Docker provider, read-only `scan` / `doctor` with
per-pair reachability and path / hardlink checks.
Catalog covers radarr, sonarr, prowlarr, transmission, jellyfin, seerr (jellyseerr/overseerr).

```sh
go test ./...                 # replays an Umbrel-shaped docker API fixture, no Docker needed
go run ./cmd/arrglue scan     # local Docker socket (DOCKER_HOST honoured); -json for machine output
go run ./cmd/arrglue doctor   # exit 1 on any problem
```

## Testing against a real umbrelOS

umbrelOS runs fine inside Docker (`dockurr/umbrel`): umbreld spawns the apps as
sibling containers on the host's Docker, on `umbrel_main_network`, with app-data
under the `/data` mount. That is the test bed.

```sh
docker run -d --name umbrel-verify -p 80:80 \
  -v ~/.umbrel-verify/data:/data -v /var/run/docker.sock:/var/run/docker.sock \
  dockurr/umbrel:2.0.0
# finish onboarding at http://localhost, then install apps via UI or CLI:
docker exec -e UMBREL_DATA_DIR=/data umbrel-verify \
  node /opt/umbreld/node_modules/.bin/tsx /opt/umbreld/source/cli.ts client apps.install.mutate --appId radarr
```

App containers have no published ports (Umbrel's ingress owns them), so from the
host `scan` finds apps and reads their config but cannot probe them. Run arrglue
inside the network instead, exactly like the future Umbrel app:

```sh
deploy/dev/umbrel.sh scan
deploy/dev/umbrel.sh doctor
```

## Testing on a real Umbrel

Every push to `main` publishes `ghcr.io/mathieuletyrant/arrglue:latest` (amd64 + arm64).
On the Umbrel, open Settings → Advanced → Terminal and run:

```sh
docker run --rm --pull always --network umbrel_main_network \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /home/umbrel/umbrel:/home/umbrel/umbrel:ro \
  ghcr.io/mathieuletyrant/arrglue doctor
```

With SSH access instead: `deploy/dev/remote.sh doctor` (cross-compiles and runs the same thing over `ssh umbrel@umbrel.local`).
