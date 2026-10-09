#!/usr/bin/env bash
# Provision the three Live Activity push secret files below a baby-tracker-sync
# deployment root (Server/deploy/unraid/live-activity-push.yaml):
#
#   secrets/live_activity_password      generated, read by the postgres container (uid 70)
#   secrets/live_activity_database_url  built from that password, read by the app (uid 65532)
#   secrets/apns_key.p8                 the operator's Apple .p8, read by the app (uid 65532)
#
# Idempotent: a rerun with the same inputs changes nothing. Existing files are
# never overwritten without --force. --dry-run prints the actions only. No
# secret value is ever printed or passed to another process's argument list.
#
# The deployment root is proven by self-naming: BT_DEPLOY_ROOT must be a
# non-symlink directory whose deployment.env (written by
# provision-unraid-secrets.sh) contains the exact line BT_DEPLOY_ROOT=<that path>.
set -euo pipefail

usage() {
  echo "usage: provision-live-activity-secrets.sh [--apns-key <path to .p8>] [--force] [--dry-run]" >&2
  exit 2
}

apns_key_source=
force=0
dry_run=0
while [[ $# -gt 0 ]]; do
  case $1 in
    --apns-key)
      [[ $# -ge 2 && -n $2 ]] || usage
      apns_key_source=$2
      shift 2
      ;;
    --force) force=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    *) usage ;;
  esac
done

# The self-naming root proof (see the header).
expected_root=/mnt/user/appdata/baby-tracker-sync
deploy_root=${BT_DEPLOY_ROOT:-$expected_root}
# A trailing slash (shell completion leaves one) names the same root; without
# this, the exact-line proof against deployment.env below would refuse it.
deploy_root=${deploy_root%/}
if [[ -L $deploy_root || ! -d $deploy_root ]]; then
  echo "BT_DEPLOY_ROOT must be the non-symlink baby-tracker-sync deployment directory: $deploy_root" >&2
  exit 1
fi
environment_file=$deploy_root/deployment.env
if [[ ! -f $environment_file || -L $environment_file ]] || ! grep -qxF "BT_DEPLOY_ROOT=$deploy_root" "$environment_file"; then
  echo "refusing a root without a deployment.env naming it (run provision-unraid-secrets.sh first): $deploy_root" >&2
  exit 1
fi
secrets=$deploy_root/secrets
if [[ ! -d $secrets || -L $secrets ]]; then
  echo "refusing unsafe secrets directory: $secrets" >&2
  exit 1
fi

# Owners the consuming containers need. BT_ALLOW_UNOWNED_SECRETS=1 lets a
# non-root run (tests on a workstation) skip chown; never use it on Unraid.
db_owner=70:70
app_owner=65532:65532
apply_ownership=1
if [[ $(id -u) -ne 0 ]]; then
  if [[ ${BT_ALLOW_UNOWNED_SECRETS:-0} != 1 ]]; then
    echo "provisioning the baby-tracker-sync push secrets requires root (the files are owned by uid 70 and uid 65532)" >&2
    exit 1
  fi
  apply_ownership=0
fi
for tool in openssl mktemp; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

umask 077
password_file=$secrets/live_activity_password
url_file=$secrets/live_activity_database_url
key_file=$secrets/apns_key.p8
principal=baby_tracker_live_activity_app
url_pattern="^postgres://$principal:[0-9a-f]{64}@db:5432/babytracker\\?sslmode=disable$"
changed=0
rotated=0

say() { printf '%s\n' "$*"; }

regular_file() { [[ -f $1 && ! -L $1 ]]; }

# Validate the source key before anything is written: a regular, non-symlink,
# unencrypted PEM private key on the P-256 curve (what Apple issues as a .p8).
# openssl reads the private key from the file itself; only the derived public
# key enters the pipe, so no private-key byte passes through another process.
# grep -c reads the whole text (grep -q would leave at the first match and,
# under pipefail, a SIGPIPE'd openssl would fail a valid key).
if [[ -n $apns_key_source ]]; then
  if ! regular_file "$apns_key_source"; then
    echo "--apns-key must name a regular non-symlink file" >&2
    exit 1
  fi
  if ! grep -q '^-----BEGIN \(EC \)\?PRIVATE KEY-----$' "$apns_key_source" ||
     ! openssl pkey -in "$apns_key_source" -pubout 2>/dev/null | openssl pkey -pubin -noout -text 2>/dev/null | grep -c 'prime256v1' >/dev/null; then
    echo "--apns-key must be an unencrypted PEM P-256 private key (Apple .p8)" >&2
    exit 1
  fi
fi

# Decide what the database pair needs.
pair_action=keep
if [[ -e $password_file || -e $url_file ]]; then
  for existing in "$password_file" "$url_file"; do
    if [[ -e $existing ]] && ! regular_file "$existing"; then
      echo "refusing unsafe existing path: $existing" >&2
      exit 1
    fi
  done
  if [[ $force -eq 1 ]]; then
    pair_action=rotate
  elif [[ ! -e $password_file || ! -e $url_file ]]; then
    echo "only one of live_activity_password and live_activity_database_url exists; rerun with --force to regenerate both" >&2
    exit 1
  else
    grep -Eq '^[0-9a-f]{64}$' "$password_file" || { echo "refusing malformed existing secret: $password_file" >&2; exit 1; }
    grep -Eq "$url_pattern" "$url_file" || { echo "refusing malformed existing database URL: $url_file" >&2; exit 1; }
    if [[ "postgres://$principal:$(<"$password_file")@db:5432/babytracker?sslmode=disable" != "$(<"$url_file")" ]]; then
      echo "live_activity_database_url does not embed live_activity_password; rerun with --force to regenerate both" >&2
      exit 1
    fi
  fi
else
  pair_action=create
fi

# Decide what the key needs.
key_action=none
if [[ -n $apns_key_source ]]; then
  if [[ -e $key_file ]]; then
    regular_file "$key_file" || { echo "refusing unsafe existing path: $key_file" >&2; exit 1; }
    if cmp -s "$apns_key_source" "$key_file"; then
      key_action=keep
    elif [[ $force -eq 1 ]]; then
      key_action=replace
    else
      echo "$key_file exists and differs from --apns-key; rerun with --force to replace it" >&2
      exit 1
    fi
  else
    key_action=install
  fi
elif [[ -e $key_file ]]; then
  regular_file "$key_file" || { echo "refusing unsafe existing path: $key_file" >&2; exit 1; }
  key_action=keep
fi

if [[ $dry_run -eq 1 ]]; then
  say "DRY RUN: nothing is written"
  case $pair_action in
    create)
      say "would create $password_file (owner $db_owner, mode 0600)"
      say "would create $url_file (owner $app_owner, mode 0600)"
      ;;
    rotate)
      say "would rotate $password_file (owner $db_owner, mode 0600)"
      say "would rotate $url_file (owner $app_owner, mode 0600)"
      ;;
    keep) say "would keep $password_file and $url_file unchanged" ;;
  esac
  case $key_action in
    install) say "would install $key_file (owner $app_owner, mode 0600) from $apns_key_source" ;;
    replace) say "would replace $key_file (owner $app_owner, mode 0600) from $apns_key_source" ;;
    keep) say "would keep $key_file unchanged" ;;
    none) say "apns_key.p8 not provided: rerun with --apns-key <path> to install it" ;;
  esac
  exit 0
fi

# Every temporary path is registered here and removed on any exit, so a
# failure between mktemp and mv leaves no partial secret behind.
cleanup_paths=()
scratch=
cleanup() {
  local path
  for path in "${cleanup_paths[@]+"${cleanup_paths[@]}"}"; do
    rm -f "$path" 2>/dev/null || true
  done
  if [[ -n $scratch ]]; then
    rm -f "$scratch"/* 2>/dev/null || true
    rmdir "$scratch" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Write a secret file atomically: a 0600 temporary file in the same directory,
# chowned before it is moved into place, so no reader ever sees a partial or
# wrongly owned file.
install_secret() {
  local destination=$1 owner=$2 source=$3
  local temporary
  temporary=$(mktemp "$secrets/.$(basename "$destination").XXXXXX")
  cleanup_paths+=("$temporary")
  cat "$source" > "$temporary"
  chmod 0600 "$temporary"
  if [[ $apply_ownership -eq 1 ]]; then
    chown "$owner" "$temporary"
  fi
  mv -f "$temporary" "$destination"
}

case $pair_action in
  create|rotate)
    scratch=$(mktemp -d "$secrets/.provision.XXXXXX")
    openssl rand -hex 32 > "$scratch/password"
    grep -Eq '^[0-9a-f]{64}$' "$scratch/password" || { echo "password generation failed" >&2; exit 1; }
    printf 'postgres://%s:%s@db:5432/babytracker?sslmode=disable\n' "$principal" "$(<"$scratch/password")" > "$scratch/url"
    install_secret "$password_file" "$db_owner" "$scratch/password"
    install_secret "$url_file" "$app_owner" "$scratch/url"
    rm -f "$scratch/password" "$scratch/url"
    if [[ $pair_action == create ]]; then
      say "created $password_file (owner $db_owner, mode 0600)"
      say "created $url_file (owner $app_owner, mode 0600)"
    else
      say "rotated $password_file (owner $db_owner, mode 0600)"
      say "rotated $url_file (owner $app_owner, mode 0600)"
      rotated=1
    fi
    changed=1
    ;;
  keep) say "$password_file and $url_file unchanged" ;;
esac

case $key_action in
  install|replace)
    install_secret "$key_file" "$app_owner" "$apns_key_source"
    if [[ $key_action == install ]]; then
      say "installed $key_file (owner $app_owner, mode 0600)"
    else
      say "replaced $key_file (owner $app_owner, mode 0600)"
    fi
    say "remove the source copy once the preflight passes: $apns_key_source"
    changed=1
    ;;
  keep) say "$key_file unchanged" ;;
  none) say "apns_key.p8 not provided: rerun with --apns-key <path> to install it" ;;
esac

# Final proof: regular files, mode 0600, and the intended owners when applied.
file_owner_mode() {
  if stat -c '%u:%g %a' "$1" >/dev/null 2>&1; then
    stat -c '%u:%g %a' "$1"
  else
    stat -f '%u:%g %Lp' "$1"
  fi
}
verify() {
  local path=$1 owner=$2 observed
  regular_file "$path" || { echo "verification failed: $path is not a regular file" >&2; exit 1; }
  observed=$(file_owner_mode "$path")
  if [[ $apply_ownership -eq 1 && $observed != "$owner 600" ]]; then
    echo "verification failed: $path is $observed, want $owner 600" >&2
    exit 1
  fi
  if [[ $apply_ownership -eq 0 && ${observed#* } != 600 ]]; then
    echo "verification failed: $path mode is ${observed#* }, want 600" >&2
    exit 1
  fi
}
verify "$password_file" "$db_owner"
verify "$url_file" "$app_owner"
[[ -e $key_file ]] && verify "$key_file" "$app_owner"

if [[ $apply_ownership -eq 0 ]]; then
  say "ownership not applied (not root): the containers need $db_owner for live_activity_password and $app_owner for the app secrets"
fi
if [[ $rotated -eq 1 ]]; then
  say "the worker password changed: recreate db with the overlay and rerun 20-init-users.sh before starting app"
fi
if [[ $changed -eq 0 ]]; then
  say "PUSH SECRETS: OK (unchanged)"
else
  say "PUSH SECRETS: OK"
fi
