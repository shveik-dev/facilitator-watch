#!/bin/sh
# Host-side orchestration only: every project operation runs in rootless Podman.
set -eu

action=${1:-test}
image=${FW_GO_IMAGE:-docker.io/library/golang:1.26}
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ "$(podman info --format '{{.Host.Security.Rootless}}')" != true ]; then
  echo 'Rootless Podman is required.' >&2
  exit 1
fi

mount_mode=ro
case "$action" in
  format|deps) mount_mode=rw ;;
  test|build|vet|scan|check-format|serve|serve-once) ;;
  *) echo 'Usage: sh dev.sh [deps|format|check-format|test|vet|build|scan|serve|serve-once]' >&2; exit 2 ;;
esac

network=none
proxy=off
sumdb=off
modmode=vendor
if [ "$action" = serve ] || [ "$action" = serve-once ]; then
  # Browser-accessible local preview. Publish only host loopback, disable
  # host-loopback access from the container, and sinkhole all fixed poll
  # targets for serve. Only explicitly authorized serve-once permits live GETs.
  network=slirp4netns:allow_host_loopback=false
fi
if [ "$action" = deps ]; then
  # The sole development-network exception: pinned Go modules via the module
  # proxy/checksum service, inside Podman. Never start the monitor here.
  network=slirp4netns
  proxy=https://proxy.golang.org
  sumdb=sum.golang.org
  modmode=mod
fi

set -- run --rm --pull=never --network="$network" --read-only \
  --userns=keep-id --cap-drop=ALL --security-opt=no-new-privileges \
  --pids-limit=256 --tmpfs /tmp:rw,nosuid,nodev,size=2g,mode=1777 \
  --volume "$source_dir:/src:$mount_mode" --workdir /src \
  --env HOME=/tmp --env GOCACHE=/tmp/go-cache --env GOPATH=/tmp/go \
  --env GOPROXY="$proxy" --env GOSUMDB="$sumdb" --env GOTOOLCHAIN=local \
  --env GOFLAGS="-mod=$modmode" \
  --env CGO_ENABLED=0 --env GOMAXPROCS=2

if [ "$action" = serve ] || [ "$action" = serve-once ]; then
  set -- "$@" --publish=127.0.0.1:8501:8080
fi
if [ "$action" = serve ]; then
  set -- "$@" \
    --add-host=facilitator.payai.network:127.0.0.1 \
    --add-host=facilitator.mogami.tech:127.0.0.1 \
    --add-host=x402.dexter.cash:127.0.0.1 \
    --add-host=api.solvador.com:127.0.0.1 \
    --add-host=x402.polygon.technology:127.0.0.1 \
    --add-host=x402.mikedotexe.com:127.0.0.1 \
    --add-host=api.mrdn.finance:127.0.0.1 \
    --add-host=x402-facilitator.cspr.cloud:127.0.0.1 \
    --add-host=api.cdp.coinbase.com:127.0.0.1
fi
set -- "$@" "$image"

case "$action" in
  format) exec podman "$@" sh -c 'gofmt -w ./*.go' ;;
  check-format) exec podman "$@" sh -c 'files=$(gofmt -l ./*.go); if [ -n "$files" ]; then printf "%s\n" "$files"; exit 1; fi' ;;
  deps) exec podman "$@" sh -ec 'mkdir /tmp/work; cp /src/*.go /src/go.mod /src/go.sum /tmp/work/; cp -R /src/web /tmp/work/web; cd /tmp/work; go mod download; go mod verify; go mod vendor; cp go.mod go.sum /src/; rm -rf /src/vendor; cp -R vendor /src/vendor' ;;
  test) exec podman "$@" go test -count=1 -v ./... ;;
  vet) exec podman "$@" go vet ./... ;;
  build) exec podman "$@" go build -trimpath -o /tmp/facilitator-watch . ;;
  scan) exec podman "$@" sh ./scripts/scan.sh ;;
  serve) exec podman "$@" sh -c 'go build -trimpath -o /tmp/facilitator-watch . && exec /tmp/facilitator-watch -listen 0.0.0.0:8080 -db /tmp/facilitators.sqlite' ;;
  serve-once) exec podman "$@" sh -c 'go build -trimpath -o /tmp/facilitator-watch . && exec /tmp/facilitator-watch -once -listen 0.0.0.0:8080 -db /tmp/facilitators.sqlite' ;;
esac
