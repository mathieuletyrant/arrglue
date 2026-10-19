#!/bin/sh
# Build a static linux binary, copy it to a real Umbrel over SSH and run it there
# inside umbrel_main_network (socket read-only, app-data mounted at its host path).
#
#   deploy/dev/remote.sh scan                      # default host umbrel@umbrel.local
#   HOST=umbrel@192.168.1.50 deploy/dev/remote.sh doctor
set -eu
cd "$(dirname "$0")/../.."
HOST=${HOST:-umbrel@umbrel.local}
ARCH=$(ssh "$HOST" uname -m)
case "$ARCH" in aarch64) GOARCH=arm64 ;; x86_64) GOARCH=amd64 ;; *) echo "unknown arch $ARCH" >&2; exit 1 ;; esac
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -trimpath -ldflags='-s -w' -o /tmp/arrglue-linux ./cmd/arrglue
scp -q /tmp/arrglue-linux "$HOST:/tmp/arrglue"
ssh -t "$HOST" "docker run --rm --network umbrel_main_network \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /home/umbrel/umbrel:/home/umbrel/umbrel:ro \
  -v /tmp/arrglue:/arrglue:ro alpine:3.20 /arrglue $*"
