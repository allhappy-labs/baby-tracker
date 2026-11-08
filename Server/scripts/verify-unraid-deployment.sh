#!/usr/bin/env bash
set -euo pipefail

project=baby-tracker-sync
expected_root=/mnt/user/appdata/baby-tracker-sync
deploy_root=${BT_DEPLOY_ROOT:-$expected_root}
baseline=$deploy_root/unrelated-containers.before.tsv
mode=${1:-verify}
if [[ $deploy_root != "$expected_root" || -L $deploy_root ]]; then
  echo "BT_DEPLOY_ROOT must be the non-symlink path $expected_root" >&2
  exit 1
fi
for tool in docker curl jq; do command -v "$tool" >/dev/null; done

snapshot_unrelated() {
  local output=$1
  local temporary=$output.tmp.$$
  : > "$temporary"
  while IFS= read -r id; do
    [[ -n $id ]] || continue
    if [[ $(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$id") == "$project" ]]; then
      continue
    fi
    docker inspect "$id" | jq -c '.[0] | {
      project:(.Config.Labels["com.docker.compose.project"] // ""),
      name:.Name,
      image:.Config.Image,
      state:.State.Status,
      ports:.HostConfig.PortBindings,
      networks:(.NetworkSettings.Networks | keys | sort)
    }' >> "$temporary"
  done < <(docker ps -aq)
  sort -o "$temporary" "$temporary"
  mv "$temporary" "$output"
}

case $mode in
  capture)
    [[ ! -e $baseline ]] || { echo "refusing to overwrite existing baseline $baseline" >&2; exit 1; }
    snapshot_unrelated "$baseline"
    echo "captured unrelated-container baseline at $baseline"
    exit 0
    ;;
  verify) ;;
  *) echo "usage: $0 [capture|verify]" >&2; exit 2 ;;
esac

[[ -f $baseline ]] || { echo "missing baseline; run $0 capture before deployment" >&2; exit 1; }
current=$(mktemp "$deploy_root/unrelated-containers.current.XXXXXXXX")
trap 'rm -f "$current"' EXIT
snapshot_unrelated "$current"
if ! diff -u "$baseline" "$current"; then
  echo "unrelated container state changed; inspect the diff before accepting deployment" >&2
  exit 1
fi

mapfile -t project_ids < <(docker ps -q --filter "label=com.docker.compose.project=$project")
[[ ${#project_ids[@]} -eq 3 ]] || { echo "expected exactly three running $project containers" >&2; exit 1; }
for id in "${project_ids[@]}"; do
  bindings=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$id")
  [[ $bindings == null || $bindings == "{}" ]] || {
    echo "project container publishes a host port: $id" >&2
    exit 1
  }
done

# Live Activity push and wakes come only from push.yaml; without it the app
# starts healthy with push off.
app_id=$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=app")
[[ -n $app_id ]]
config_files=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$app_id")
[[ ,$config_files, == *",$deploy_root/source/Server/deploy/unraid/push.yaml,"* ]] || {
  echo "app was not composed with source/Server/deploy/unraid/push.yaml: $config_files" >&2
  exit 1
}
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$app_id" | grep -qx 'BT_LIVE_ACTIVITY_PUSH_ENABLED=true' || {
  echo "app runs without BT_LIVE_ACTIVITY_PUSH_ENABLED=true" >&2
  exit 1
}

tailscale_id=$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=tailscale")
[[ -n $tailscale_id ]]
docker exec "$tailscale_id" tailscale status --peers=false >/dev/null
# The node's own MagicDNS name: the name Serve holds the certificate for.
server_name=$(docker exec "$tailscale_id" tailscale status --json --peers=false | jq -r '.Self.DNSName | rtrimstr(".")')
[[ -n $server_name && $server_name != null ]]
tailscale_ip=$(docker exec "$tailscale_id" tailscale ip -4)
[[ -n $tailscale_ip ]]
curl_flags=(--silent --show-error --connect-timeout 5 --max-time 15 --resolve "$server_name:443:$tailscale_ip" -o /dev/null -w '%{http_code}')
[[ $(curl "${curl_flags[@]}" "https://$server_name/healthz") == 200 ]]
[[ $(curl "${curl_flags[@]}" "https://$server_name/readyz") == 200 ]]
# The phones' live socket: a refused bearer proves the upgrade reaches the app
# through Serve over HTTP/1.1, with no session needed.
live_flags=(--http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13'
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Sec-WebSocket-Protocol: baby-tracker.v1'
  -H 'X-Baby-Tracker-Protocol: v1' -H 'X-Baby-Tracker-Capabilities: records-v1,live-hints-v1'
  -H 'Authorization: Bearer verify-unraid-deployment')
[[ $(curl "${curl_flags[@]}" "${live_flags[@]}" "https://$server_name/v1/live") == 401 ]]
echo "deployment proof: private HTTPS health/readiness 200; live socket upgrade reaches the app (401 for a bogus bearer); app composed with push.yaml, Live Activity push on; no host ports; unrelated container snapshot unchanged"
