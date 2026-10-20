# arrglue

**Discovers your *arr stack, checks how the apps reach each other, and wires them together
through their own APIs.** Read-only by default, Terraform-style `plan` / `apply` when you want it
to write, and a registry other tools can ask for URLs and keys.

Every *arr setup is the same chore: for each pair of apps, find the right URL *from the caller's
point of view*, the callee's API key, and the matching file paths. arrglue reads all three from
the running containers and their config files, then asks the apps themselves to test what they
have configured.

```
APPS    discovered apps, as reached by arrglue
  ✓  radarr_server_1        radarr        6.4.4.10685  key ✓  http://radarr_server_1:7878
ROUTES  which app can call which, and with what URL
  ✓  prowlarr_server_1 → radarr_server_1   http://radarr_server_1:7878
PATHS   download dirs vs library folders, hardlinks
  ✓  radarr_server_1   /downloads/movies ← /downloads/complete   hardlinks possible
LINKS   what the apps have configured, tested by the apps themselves
  ✓  prowlarr_server_1 → radarr_server_1   application "radarr_server_1"   tested by prowlarr_server_1: connection and key OK
```

## What it does

- **scan**: lists the apps found on the Docker host: kind, version, networks, ports, whether a key was read.
- **doctor**: four checks, nothing written. Apps reachable with their key; a URL per caller → callee
  pair (same Docker network, published port, host gateway…); download dirs vs library folders, with
  the hardlink verdict (same filesystem, same mount); and every configured link, tested by the app
  that holds it (Prowlarr tests its applications, Radarr its download client…).
- **plan / apply**: computes the links the apps should have, shows the diff, writes through the APIs
  after confirmation. Objects arrglue creates are named with an `[arrglue]` prefix (never a tag: in Radarr, Sonarr and
  Prowlarr a tag filters what the object applies to); what you
  set up by hand is reported and left alone unless `--adopt`. A stale key is detected by the app's
  own test and re-sent.
- **serve**: daemon that rescans on Docker events and on a timer, with a web UI (rescan, tick
  the changes to apply, take over hand-made objects one by one, health checks) and a JSON
  registry: `GET /v1/services/radarr_server_1?for=prowlarr_server_1` answers with the URL that
  caller should use, and the key for the bearer of `ARRGLUE_TOKEN`.

## Apps covered

| App | Read | Links arrglue maintains |
|---|---|---|
| Radarr, Sonarr | `config.xml` | → Transmission (download client + remote path mapping), → Jellyfin (library refresh), ↔ Prowlarr |
| Prowlarr | `config.xml` | → Radarr, Sonarr (applications, both URLs) |
| Transmission | `settings.json` | callee |
| Jellyfin | `network.xml`, key in `jellyfin.db` | callee |
| Seerr / Jellyseerr / Overseerr | `settings.json` | → Radarr, Sonarr (servers, profile and root folder picked), → Jellyfin (host) |
| Cleanuparr | key in `users.db` | → Radarr, Sonarr (instances), → Transmission (download client) |
| Maintainerr | API | → Radarr, Sonarr, → Jellyfin (first admin user), → Seerr |
| Healarr | key from `--secrets` (encrypted at rest) | → Radarr, Sonarr (*arr instances) |
| Spotarr | `API_TOKEN` env | → Seerr, → Jellyfin (settings sections) |
| Bazarr | `config.yaml` | → Sonarr, Radarr (connections, tested through Bazarr's status) |
| Lingarr | key from `--secrets` unless its auth is off | → Radarr, Sonarr (settings) |
| Profilarr | reads its database | → Radarr, Sonarr: **manual**, its public API cannot write instances |
| cross-seed | `config.js` | → Prowlarr (torznab per indexer), Radarr, Sonarr, Transmission: **manual**, plan prints the exact lines |

Adding an app that only needs discovery is a YAML file in [`catalog/`](catalog/): image patterns,
config file and fields (XML, JSON, YAML, INI or regex), where the key lives (config field, SQLite
query, env var), health endpoint. Connectors that write are Go, in [`internal/connector/`](internal/connector/).

## Install

### Umbrel

Available in [Mathieu's community app store](https://github.com/mathieuletyrant/umbrel-community-app-store)
as **arrglue**: the daemon behind Umbrel's login on port 8797, the Docker API through a read-only
socket proxy, the Umbrel root mounted read-only. The app's password is the registry token. Keys
arrglue cannot read itself go in `~/umbrel/app-data/mathieu-arrglue/data/secrets.yaml`:

```yaml
mathieu-healarr_server_1: <Healarr → Config → API key>
```

Open the app: the **Plan** tab lists what arrglue would change; tick what you want and press
**Apply**. The CLI is still there from Settings → Advanced → Terminal:
`docker exec mathieu-arrglue_server_1 /arrglue plan`.

### Docker

Images: `ghcr.io/mathieuletyrant/arrglue` (amd64, arm64), tagged by release and `latest`.
Run it on the network your apps share, with their config dirs mounted read-only at the same
paths Docker reports (so what `/containers/json` says is readable as-is):

```sh
docker run -d --name arrglue --restart unless-stopped --network <your apps' network> \
  -p 8787:8787 -e ARRGLUE_TOKEN=change-me \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /srv/apps:/srv/apps:ro \
  ghcr.io/mathieuletyrant/arrglue serve
```

One-off commands: same mounts, `… ghcr.io/mathieuletyrant/arrglue doctor` (or `plan`, `apply`).
From the host instead of a container, arrglue reaches the apps through their published ports.

### CLI

```sh
go install github.com/mathieuletyrant/arrglue/cmd/arrglue@latest
arrglue scan            # --json for machine output
arrglue doctor          # exit 1 on any error
arrglue plan [--adopt] [--only jellyfin]
arrglue apply [--adopt] [--yes] [--only …]
arrglue serve [--listen :8787] [--interval 15m] [--auto-apply]
```

Flags: `--catalog <dir>` overrides catalog entries, `--secrets <file>` hands over keys
(`{<container name>: <key>}`), `DOCKER_HOST` picks the socket or a `tcp://` proxy,
`ARRGLUE_DEBUG=1` logs the duration of every HTTP call.

## Registry API

| Endpoint | Returns |
|---|---|
| `GET /` | web UI |
| `GET /v1/state` | everything the web UI shows: last cycle, scanning or not, report, plan |
| `POST /v1/scan` | rescan now (202) |
| `POST /v1/apply` | `{"keys": [...]}`, keys from the plan: plans again and writes those still in it |
| `GET /v1/services` | discovered apps and the URL arrglue reaches them at |
| `GET /v1/services/{id-or-name}?for=<caller>` | the URL *that caller* should use, verified or not |
| `GET /v1/doctor`, `GET /v1/plan` | last doctor report and plan, as JSON |
| `GET /v1/status`, `GET /healthz` | last cycle time and counts |

The `api_key` field is only included with `Authorization: Bearer $ARRGLUE_TOKEN`. Nothing is
written unless someone applies from the web UI (or `POST /v1/apply`), or with `serve --auto-apply`,
which never takes over a hand-made object without `--adopt`. The POST endpoints only take JSON
from the page's own origin, so another site cannot make your browser call them. Keep the port on your internal network: arrglue holds every
key of the stack.

## How it decides

- **Routes**: a URL is only valid for a given caller. Candidates, best first: shared Docker network
  (`http://<container>:<port>`), `127.0.0.1:<published port>` for host-mode callers, the caller's
  bridge gateway or `host.docker.internal` for published ports. Two stacks on one host are never
  cross-wired: pairs need a common network. arrglue verifies a route when it shares the caller's
  vantage point (same network, or both on the host) and says *unverified* otherwise.
- **Paths**: every mount is known, so a path seen by one app is translated to the host and to the
  other app. Different paths for the same dir → remote path mapping; different filesystems or
  separate mounts of the same disk → no hardlinks, said before a 50 GB copy happens.
- **Keys**: read from config files, SQLite databases (WAL journal included, the app may hold it
  open), container env, or your secrets file. When several keys are found, the one the app accepts
  wins. Masked values the APIs return on read count as unchanged, unless the app's own test fails.

## Development

```sh
go test ./...                       # unit tests replay a Docker API fixture, no Docker needed
cd deploy/dev/docker && docker compose up -d   # plain-Docker test bed: bridge, published ports, separate mounts
deploy/dev/umbrel.sh doctor         # against umbrelOS running in Docker (dockurr/umbrel), see deploy/dev/
```

The design is in [SPEC.md](SPEC.md). Not done yet: ephemeral per-caller probe (`--network
container:<caller>`), native and manual providers, SQLite history with encrypted keys, MCP server.

## License

MIT
