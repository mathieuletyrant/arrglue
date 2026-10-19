#!/bin/sh
# Run arrglue the way the Umbrel app will: inside umbrel_main_network, Docker
# socket read-only, app-data mounted at its host path so Docker's mount sources
# are readable as-is. Targets an umbrelOS-in-Docker instance (dockurr/umbrel).
#
#   deploy/dev/umbrel.sh scan
#   deploy/dev/umbrel.sh doctor
#   UMBREL=myumbrel deploy/dev/umbrel.sh scan      # another container name
set -eu
cd "$(dirname "$0")/../.."
UMBREL=${UMBREL:-umbrel-verify}
DATA=$(docker inspect "$UMBREL" --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}')
[ -n "$DATA" ] || { echo "cannot find /data mount of container $UMBREL" >&2; exit 1; }
docker build -q -t arrglue:dev . >/dev/null
exec docker run --rm --network umbrel_main_network \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$DATA/app-data:$DATA/app-data:ro" \
  arrglue:dev "$@"
