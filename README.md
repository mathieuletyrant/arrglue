# arrglue

Discovers the *arr stack wherever it runs, works out how each app can reach the
others, and wires them together through their APIs. Spec: [SPEC.md](SPEC.md).

Status: roadmap step 4 — `scan` / `doctor` (read-only: discovery, per-pair reachability,
path / hardlink checks, links tested by the apps themselves), `plan` / `apply`
(connectors: prowlarr → radarr/sonarr, radarr/sonarr → transmission + remote path
mapping, seerr → radarr/sonarr/jellyfin, radarr/sonarr → jellyfin library refresh)
and `serve` (daemon + registry API). Every object arrglue creates is
tagged `arrglue`; equivalent objects made by hand are left alone unless `-adopt`.
Catalog covers radarr, sonarr, prowlarr, transmission, jellyfin, seerr (jellyseerr/overseerr),
cleanuparr, maintainerr, healarr, spotarr and cross-seed.

```sh
go test ./...                 # replays an Umbrel-shaped docker API fixture, no Docker needed
go run ./cmd/arrglue scan     # local Docker socket (DOCKER_HOST honoured); -json for machine output
go run ./cmd/arrglue doctor   # APPS / ROUTES / PATHS / LINKS sections, exit 1 on any error
go run ./cmd/arrglue plan     # Terraform-style diff of the links, writes nothing
go run ./cmd/arrglue apply    # asks before writing (--yes to skip, --adopt to take over hand-made objects)
go run ./cmd/arrglue serve    # daemon: rescans on Docker events and every 15 min, serves the registry
```

## Connectors

| From → to | Object | Ownership |
|---|---|---|
| prowlarr → radarr/sonarr | application (both URLs) | `arrglue` tag |
| radarr/sonarr → transmission | download client + remote path mapping | `arrglue` tag |
| radarr/sonarr → jellyfin | library refresh notification | `arrglue` tag |
| seerr → radarr/sonarr, jellyfin | servers, media server host | `[arrglue]` name prefix / slot |
| maintainerr → radarr/sonarr, jellyfin, seerr | servers, media server (first Jellyfin admin), seerr | `[arrglue]` prefix / slot |
| cleanuparr → radarr/sonarr, transmission | instances, download client | `[arrglue]` prefix |
| healarr → radarr/sonarr | *arr instances | `[arrglue]` prefix |
| spotarr → seerr, jellyfin | settings sections | slot |
| cross-seed → prowlarr, radarr, sonarr, transmission | `config.js` arrays | **manual**: plan prints what to add |

A *slot* is a single-valued setting: created when empty, left alone when set by hand unless `--adopt`.

Keys arrglue cannot read itself (Healarr encrypts its own) go in a YAML file passed with
`--secrets` or `ARRGLUE_SECRETS`: `{<container name>: <key>}`. Apps still in their first-run
wizard (Seerr, Cleanuparr, Healarr) are reported as pending until you finish it once.

## Registry API (`serve`)

| Endpoint | Returns |
|---|---|
| `GET /` | web page: last doctor report and plan, refreshed every minute |
| `GET /v1/services` | discovered apps and the URL arrglue reaches them at |
| `GET /v1/services/{id-or-name}?for=<caller>` | the URL *that caller* should use, verified or not |
| `GET /v1/doctor`, `GET /v1/plan` | last doctor report and plan, as JSON |
| `GET /v1/status`, `GET /healthz` | last cycle time and counts |

The API key of a service is only included when the request carries
`Authorization: Bearer $ARRGLUE_TOKEN`. Nothing is written unless `--auto-apply`.
Keep the port on your internal network: arrglue holds every key of the stack.

## Test beds

### Plain Docker (bridge network, published ports, separate mounts)

```sh
cd deploy/dev/docker && docker compose up -d && cd -
go run ./cmd/arrglue doctor      # host vantage: 127.0.0.1:<published port>
go run ./cmd/arrglue plan
docker build -t arrglue:dev . && docker run --rm --network arr-dev_arr \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v $PWD/deploy/dev/docker/data:$PWD/deploy/dev/docker/data:ro arrglue:dev doctor   # container vantage
```

### Real umbrelOS in Docker

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

## As an Umbrel app

Packaged in [Mathieu's community app store](https://github.com/mathieuletyrant/umbrel-community-app-store)
as `mathieu-arrglue`: `serve` behind Umbrel's login, Docker API through a read-only socket
proxy, the Umbrel root mounted read-only, `ARRGLUE_TOKEN` = the app's password, secrets in
`~/umbrel/app-data/mathieu-arrglue/data/secrets.yaml`. Writes still go through
`docker exec mathieu-arrglue_server_1 /arrglue apply …` from the Umbrel terminal.

## Testing on a real Umbrel

Every push to `main` publishes `ghcr.io/mathieuletyrant/arrglue:latest` (amd64 + arm64).
On the Umbrel, open Settings → Advanced → Terminal and run:

```sh
docker run --rm --pull always --network umbrel_main_network \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /home/umbrel/umbrel:/home/umbrel/umbrel:ro \
  ghcr.io/mathieuletyrant/arrglue doctor
```

As a daemon (registry on port 8787 of the Umbrel, keys only with the token):

```sh
docker run -d --name arrglue --restart unless-stopped --network umbrel_main_network \
  -p 8787:8787 -e ARRGLUE_TOKEN=change-me \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /home/umbrel/umbrel:/home/umbrel/umbrel:ro \
  ghcr.io/mathieuletyrant/arrglue serve
```

With SSH access instead: `deploy/dev/remote.sh doctor` (cross-compiles and runs the same thing over `ssh umbrel@umbrel.local`).
