#!/usr/bin/env bash
set -euo pipefail

expected_root=/mnt/user/appdata/baby-tracker-sync
deploy_root=${BT_DEPLOY_ROOT:-$expected_root}
if [[ $deploy_root != "$expected_root" || -L $deploy_root ]]; then
  echo "BT_DEPLOY_ROOT must be the non-symlink path $expected_root" >&2
  exit 1
fi
if [[ $(id -u) -ne 0 ]]; then
  echo "provisioning the baby-tracker-sync deployment requires root" >&2
  exit 1
fi
for tool in openssl uuidgen install; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

umask 077
install -d -m 0700 "$deploy_root" "$deploy_root/secrets" "$deploy_root/tailscale-state" "$deploy_root/tailscale-run"
install -d -m 0700 -o 70 -g 70 "$deploy_root/database"
install -d -m 0700 -o 65532 -g 65532 "$deploy_root/keyrings"

create_hex_secret() {
  local destination=$1
  if [[ -e $destination ]]; then
    [[ -f $destination && ! -L $destination ]] || { echo "refusing unsafe existing path: $destination" >&2; exit 1; }
    grep -Eq '^[0-9a-f]{64}$' "$destination" || { echo "refusing malformed existing secret: $destination" >&2; exit 1; }
    return
  fi
  openssl rand -hex 32 > "$destination"
  chmod 0444 "$destination"
}

for name in bootstrap_password migrator_password runtime_password export_password \
  identity_pepper session_token_pepper device_continuity_pepper apple_notification_pepper \
  invitation_pepper page_token_key actor_id_pepper rate_limit_pepper; do
  create_hex_secret "$deploy_root/secrets/$name"
done

create_database_url() {
  local destination=$1 principal=$2 password_file=$3
  if [[ -e $destination ]]; then
    [[ -f $destination && ! -L $destination ]] || { echo "refusing unsafe existing path: $destination" >&2; exit 1; }
    grep -Eq "^postgres://$principal:[0-9a-f]{64}@db:5432/babytracker\\?sslmode=disable$" "$destination" || {
      echo "refusing malformed existing database URL: $destination" >&2
      exit 1
    }
    return
  fi
  printf 'postgres://%s:%s@db:5432/babytracker?sslmode=disable\n' \
    "$principal" "$(<"$password_file")" > "$destination"
  chmod 0444 "$destination"
}

create_database_url "$deploy_root/secrets/migrator_database_url" baby_tracker_migrator "$deploy_root/secrets/migrator_password"
create_database_url "$deploy_root/secrets/runtime_database_url" baby_tracker_app "$deploy_root/secrets/runtime_password"
# The maintenance credential compose mounts by default (decision D3).
create_database_url "$deploy_root/secrets/maintenance_database_url" baby_tracker_export_app "$deploy_root/secrets/export_password"

export_database_url=$deploy_root/keyrings/export-database-url
if [[ ! -e $export_database_url ]]; then
  printf 'postgres://baby_tracker_export_app:%s@db:5432/babytracker?sslmode=disable\n' \
    "$(<"$deploy_root/secrets/export_password")" > "$export_database_url"
  chown 65532:65532 "$export_database_url"
  chmod 0600 "$export_database_url"
fi
[[ -f $export_database_url && ! -L $export_database_url ]] || { echo "unsafe export database URL" >&2; exit 1; }

export_keys=$deploy_root/keyrings/export-keys.json
if [[ ! -e $export_keys ]]; then
  printf '{"export.v1":"%s"}\n' "$(openssl rand -hex 32)" > "$export_keys"
  chown 65532:65532 "$export_keys"
  chmod 0600 "$export_keys"
fi
[[ -f $export_keys && ! -L $export_keys ]] || { echo "unsafe export keyring" >&2; exit 1; }

auth_key_source=${BT_TAILSCALE_AUTH_KEY_FILE_SOURCE:-}
auth_key_destination=$deploy_root/secrets/tailscale_auth_key
if [[ -n $auth_key_source && ! -e $auth_key_destination ]]; then
  [[ -f $auth_key_source && ! -L $auth_key_source ]] || { echo "invalid Tailscale auth-key source" >&2; exit 1; }
  install -m 0400 "$auth_key_source" "$auth_key_destination"
fi

environment_file=$deploy_root/deployment.env
source_sha=${BT_SOURCE_SHA:-}
[[ $source_sha =~ ^[0-9a-f]{40}$ ]] || { echo "BT_SOURCE_SHA must be the reviewed 40-character Git SHA" >&2; exit 1; }
if [[ ! -e $environment_file ]]; then
  instance_id=$(uuidgen | tr '[:upper:]' '[:lower:]')
  {
    printf 'BT_DEPLOY_ROOT=%s\n' "$deploy_root"
    printf 'BT_SECRETS_DIR=%s/secrets\n' "$deploy_root"
    printf 'BT_SERVER_INSTANCE_ID=%s\n' "$instance_id"
    printf 'BT_RECOVERY_EPOCH=1\n'
    printf 'BT_MODE=self_hosted\n'
    printf 'BT_PUBLIC_ORIGIN=https://baby-sync.example-tailnet.ts.net\n'
    printf 'BT_APPLE_AUDIENCE=com.olhapi.babytracker\n'
    printf 'BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32\n'
    printf 'BT_SOURCE_SHA=%s\n' "$source_sha"
    printf 'BT_IMAGE_NAME=baby-tracker-server:%s\n' "$source_sha"
    printf 'BT_KEYRING_DIR=%s/keyrings\n' "$deploy_root"
    printf 'BT_EXPORT_DATABASE_URL_FILE=/run/keyrings/export-database-url\n'
    printf 'BT_EXPORT_KEYS_FILE=/run/keyrings/export-keys.json\n'
    printf 'BT_EXPORT_KEY_ID=export.v1\n'
  } > "$environment_file"
  chmod 0600 "$environment_file"
else
  [[ -f $environment_file && ! -L $environment_file ]] || { echo "refusing unsafe deployment.env" >&2; exit 1; }
  if grep -q '^BT_SOURCE_SHA=' "$environment_file"; then
    if ! grep -qx "BT_SOURCE_SHA=$source_sha" "$environment_file"; then
      if [[ ${BT_ALLOW_SOURCE_UPDATE:-0} != 1 ]]; then
        echo "deployment.env names a different source SHA; set BT_ALLOW_SOURCE_UPDATE=1 for a reviewed upgrade" >&2
        exit 1
      fi
      environment_update=$(mktemp "$deploy_root/.deployment.env.XXXXXX")
      awk '!/^BT_SOURCE_SHA=/ && !/^BT_IMAGE_NAME=/' "$environment_file" > "$environment_update"
      printf 'BT_SOURCE_SHA=%s\nBT_IMAGE_NAME=baby-tracker-server:%s\n' "$source_sha" "$source_sha" >> "$environment_update"
      chmod 0600 "$environment_update"
      mv "$environment_update" "$environment_file"
    fi
  else
    printf 'BT_SOURCE_SHA=%s\nBT_IMAGE_NAME=baby-tracker-server:%s\n' "$source_sha" "$source_sha" >> "$environment_file"
  fi
  if ! grep -q '^BT_EXPORT_KEYS_FILE=' "$environment_file"; then
    if ! grep -q '^BT_KEYRING_DIR=' "$environment_file"; then
      printf 'BT_KEYRING_DIR=%s/keyrings\n' "$deploy_root" >> "$environment_file"
    fi
    printf 'BT_EXPORT_DATABASE_URL_FILE=/run/keyrings/export-database-url\nBT_EXPORT_KEYS_FILE=/run/keyrings/export-keys.json\nBT_EXPORT_KEY_ID=export.v1\n' >> "$environment_file"
  fi
fi

echo "baby-tracker-sync storage and secrets are provisioned at $deploy_root"
if [[ ! -s $auth_key_destination ]]; then
  echo "Tailscale enrollment remains pending: set BT_TAILSCALE_AUTH_KEY_FILE_SOURCE and rerun" >&2
fi
