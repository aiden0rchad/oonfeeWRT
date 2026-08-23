#!/bin/sh
# Exercise a clean container backup and controlled restore without a network.
set -eu

cd "$(dirname "$0")/.."

version=${1:-}
if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$'; then
  echo "usage: tools/release-container-smoke.sh <strict-semver-tag>" >&2
  exit 2
fi

for command in docker go openssl; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "release container smoke: missing $command" >&2
    exit 1
  }
done
[ "$(docker info --format '{{.OSType}}' 2>/dev/null)" = linux ] || {
  echo "release container smoke: a running Linux Docker daemon is required" >&2
  exit 1
}
daemon_arch=$(docker info --format '{{.Architecture}}' 2>/dev/null)
case "$daemon_arch" in
  amd64|x86_64) smoke_arch=amd64 ;;
  arm64|aarch64) smoke_arch=arm64 ;;
  *)
    echo "release container smoke: unsupported Docker architecture: $daemon_arch" >&2
    exit 1
    ;;
esac

port=${OONFEE_RELEASE_SMOKE_PORT:-18082}
case "$port" in
  ''|*[!0-9]*)
    echo "release container smoke: invalid port" >&2
    exit 1
    ;;
esac
[ "$port" -ge 1024 ] && [ "$port" -le 65535 ] && [ "$port" -ne 8080 ] || {
  echo "release container smoke: choose a non-8080 port from 1024 through 65535" >&2
  exit 1
}

run_id="$$-$(date +%s)"
image="oonfeewrt-release-smoke:$run_id"
container="oonfeewrt-release-smoke-$run_id"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/oonfeewrt-container-smoke.XXXXXX")
data_dir="$tmp/data"
runtime_file="$tmp/runtime-passphrase"
config_file="$tmp/config.json"
helper="$tmp/releasecontainersmoke"
runtime_passphrase=$(openssl rand -hex 24)
owner_password=$(openssl rand -hex 24)
viewer_password=$(openssl rand -hex 24)
export_passphrase=$(openssl rand -hex 24)

clear_file() {
  [ ! -f "$1" ] || : > "$1"
}
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  clear_file "$config_file"
  clear_file "$runtime_file"
  runtime_passphrase= owner_password= viewer_password= export_passphrase=
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

umask 077
mkdir "$data_dir"
printf '%s\n' "$runtime_passphrase" > "$runtime_file"
{
  printf '{\n'
  printf '  "base_url":"http://127.0.0.1:%s/api/v1",\n' "$port"
  printf '  "owner_username":"release-owner",\n'
  printf '  "owner_password":"%s",\n' "$owner_password"
  printf '  "viewer_username":"post-backup",\n'
  printf '  "viewer_password":"%s",\n' "$viewer_password"
  printf '  "export_passphrase":"%s",\n' "$export_passphrase"
  printf '  "runtime_passphrase":"%s"\n' "$runtime_passphrase"
  printf '}\n'
} > "$config_file"
chmod 0700 "$data_dir"
chmod 0600 "$runtime_file" "$config_file"

run_uid=$(id -u)
run_gid=$(id -g)
if [ "$run_uid" -eq 0 ]; then
  run_uid=65532
  run_gid=65532
  chown "$run_uid:$run_gid" "$data_dir" "$runtime_file" "$config_file"
fi

CGO_ENABLED=0 GOOS=linux GOARCH="$smoke_arch" go build -trimpath -buildvcs=false \
  -ldflags '-s -w -buildid=' -o "$helper" ./tools/releasecontainersmoke
chmod 0555 "$helper"

docker buildx build --load --platform "linux/$smoke_arch" \
  --build-arg "VERSION=$version" -f deploy/Dockerfile -t "$image" . >/dev/null

[ "$(docker image inspect --format '{{.Config.User}}' "$image")" = "65532:65532" ] || {
  echo "release container smoke: image default user is not 65532:65532" >&2
  exit 1
}
image_size=$(docker image inspect --format '{{.Size}}' "$image")
[ "$image_size" -le 41943040 ] || {
  echo "release container smoke: image exceeds the 41943040-byte limit" >&2
  exit 1
}
[ "$(docker run --rm --platform "linux/$smoke_arch" "$image" -version)" = "$version" ] || {
  echo "release container smoke: embedded version mismatch" >&2
  exit 1
}

docker run --detach --name "$container" --platform "linux/$smoke_arch" --network none \
  --user "$run_uid:$run_gid" \
  --read-only --tmpfs "/tmp:rw,noexec,nosuid,nodev,uid=$run_uid,gid=$run_gid,mode=0700" \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --mount "type=bind,source=$data_dir,target=/data" \
  --mount "type=bind,source=$runtime_file,target=/run/secrets/oonfee-passphrase,readonly" \
  --mount "type=bind,source=$config_file,target=/run/secrets/release-smoke.json,readonly" \
  --mount "type=bind,source=$helper,target=/releasecontainersmoke,readonly" \
  --env OONFEE_DATA_DIR=/data \
  --env "OONFEE_LISTEN=127.0.0.1:$port" \
  --env OONFEE_PASSPHRASE_FILE=/run/secrets/oonfee-passphrase \
  "$image" >/dev/null

helper_status=0
docker exec "$container" /releasecontainersmoke /run/secrets/release-smoke.json || helper_status=$?
clear_file "$config_file"
if [ "$helper_status" -ne 0 ]; then
  exit "$helper_status"
fi

docker stop --time 160 "$container" >/dev/null
[ "$(docker inspect --format '{{.State.ExitCode}}' "$container")" = 0 ] || {
  echo "release container smoke: controller did not stop cleanly" >&2
  exit 1
}
