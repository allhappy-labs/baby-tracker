#!/usr/bin/env bash
# Signing readiness for the family Live Activity push feature.
#
# ActivityKit push tokens belong to the app, so the app's provisioning profile
# must carry `aps-environment`; the widget extension needs a valid profile but
# no push entitlement. Today the installed manual profiles lack the key, so the
# project signs device builds with Apps/BabyTracker.entitlements and selects
# Apps/BabyTrackerLiveActivityPush.entitlements for the simulator SDK only.
# Runbook: docs/runbooks/family-live-activity-push.md, P1 and P2.
#
#   check [--app <BabyTracker.app>]
#       Read-only. Resolves PROVISIONING_PROFILE_SPECIFIER for every
#       configuration of the app and widget targets, decodes the matching
#       installed profiles and reports name, UUID, team, app ID, expiry and
#       aps-environment. Also checks that the signing identity each
#       configuration pins for device builds (CODE_SIGN_IDENTITY[sdk=iphoneos*])
#       is present and valid in the keychain, by name only
#       (`security find-identity -v -p codesigning`, read-only). With --app,
#       also inspects the signed bundle's entitlements, its embedded profile,
#       and what the app's resolver (ProvisioningProfileEnvironmentResolver)
#       will decide.
#       Exit 0 ready, 1 blocked, 2 usage or tool error.
#   enable [--dry-run]
#       Refuses unless `check` passes for every device configuration of the
#       app target. Then applies runbook P2: both app configurations sign with
#       the push entitlements, the simulator-only conditional goes, and the
#       pinning Swift test asserts the new state. --dry-run prints the diff and
#       writes nothing. Re-running on a switched project is a no-op.
#   state
#       Prints pre-switch | enabled; exit 1 on an unrecognised project state.
#
# Never prints certificates or whole profiles: of a signing identity, only its
# name. Reads, never writes, the profile directories and the keychain.
# Environment overrides exist for the fixture tests:
#   BTLAP_PROFILE_DIRS   colon-separated profile directories
#   BTLAP_PROJECT        path to project.pbxproj
#   BTLAP_PINNING_TEST   path to LiveActivityPushEligibilityTests.swift
#   BTLAP_NOW            "now" as epoch seconds, for expiry checks
#   BTLAP_CODESIGN       codesign executable
#   BTLAP_FIND_IDENTITY  command listing the valid codesigning identities, in
#                        `security find-identity -v -p codesigning` format
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)

PROJECT=${BTLAP_PROJECT:-$repo/BabyTracker.xcodeproj/project.pbxproj}
PINNING_TEST=${BTLAP_PINNING_TEST:-$repo/Packages/BabyTracker/Tests/BabySyncTests/LiveActivityPushEligibilityTests.swift}
PROFILE_DIRS=${BTLAP_PROFILE_DIRS:-$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles:$HOME/Library/MobileDevice/Provisioning Profiles}
NOW=${BTLAP_NOW:-$(date +%s)}
CODESIGN=${BTLAP_CODESIGN:-codesign}
FIND_IDENTITY=${BTLAP_FIND_IDENTITY:-}

APP_TARGET=BabyTracker
WIDGET_TARGET=BabyTrackerWidgets
BASE_ENTITLEMENTS=Apps/BabyTracker.entitlements
PUSH_ENTITLEMENTS=Apps/BabyTrackerLiveActivityPush.entitlements
SIMULATOR_KEY='CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]'
OLD_PIN=pushEntitlementsAreSelectedOnlyForTheSimulatorSDK
NEW_PIN=pushEntitlementsSignEveryAppConfiguration
SIMULATOR=${BTLAP_SIMULATOR:-'platform=iOS Simulator,name=iPhone 17 Pro'}

# The push entitlements file pins the value the profile must carry
# (pushEntitlementsAreExactlyTheAppEntitlementsPlusTheDevelopmentGateway).
project_root=$(cd "$(dirname "$PROJECT")/.." && pwd)
REQUIRED_APS=development
if [ -f "$project_root/$PUSH_ENTITLEMENTS" ]; then
  REQUIRED_APS=$(plutil -extract aps-environment raw -o - "$project_root/$PUSH_ENTITLEMENTS" 2>/dev/null || echo development)
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/btlap-signing.XXXXXX")
trap 'rm -rf "$work"' EXIT

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") check [--app <path-to-BabyTracker.app>]
       $(basename "$0") enable [--dry-run]
       $(basename "$0") state
EOF
  exit 2
}

die() {
  echo "error: $*" >&2
  exit 2
}

# ------------------------------------------------------------ project.pbxproj

# The build configuration list id of a native target.
pbx_target_config_list() { # target-name
  awk -v target="$1" '
    /^\t\t[A-F0-9]+ \/\* .* \*\/ = \{$/ { isa = ""; name = ""; list = "" }
    /^\t\t\tisa = / { isa = $3; sub(/;$/, "", isa) }
    /^\t\t\tname = / { name = $3; sub(/;$/, "", name); gsub(/"/, "", name) }
    /^\t\t\tbuildConfigurationList = / { list = $3 }
    /^\t\t\};$/ { if (isa == "PBXNativeTarget" && name == target) print list }
  ' "$PROJECT"
}

# "id<TAB>name" per configuration in a configuration list.
pbx_config_ids() { # list-id
  awk -v list="$1" '
    $0 ~ ("^\t\t" list " /\\*") { inside = 1; next }
    inside && /^\t\t\};$/ { inside = 0 }
    inside && /^\t\t\t\t[A-F0-9]+ \/\* .* \*\/,$/ { print $1 "\t" $3 }
  ' "$PROJECT"
}

# One build setting of one configuration, quotes stripped; empty when unset.
# Each "$(NAME)" in the value is resolved from the project's xcconfigs,
# Config/Local.xcconfig first (it overrides Base), as Xcode would.
pbx_setting() { # config-id key
  local value name resolved found file line rounds=0
  value=$(pbx_raw_setting "$1" "$2")
  while [[ $value =~ \$\(([A-Za-z0-9_]+)\) ]] && (( rounds++ < 8 )); do
    name=${BASH_REMATCH[1]}
    resolved= found=
    for file in "$(dirname "$PROJECT")/../Config/Local.xcconfig" "$(dirname "$PROJECT")/../Config/Base.xcconfig"; do
      [ -f "$file" ] || continue
      line=$(grep -E "^[[:space:]]*$name[[:space:]]*=" "$file" | tail -1) || continue
      resolved=$(printf '%s' "${line#*=}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
      found=1
      break
    done
    [ -n "$found" ] || break
    value=${value//"\$($name)"/$resolved}
  done
  printf '%s\n' "$value" | awk NF
}

pbx_raw_setting() { # config-id key
  awk -v id="$1" -v key="$2" '
    $0 ~ ("^\t\t" id " /\\*") { inside = 1; next }
    inside && /^\t\t\};$/ { inside = 0 }
    inside && /^\t\t\t\t[^\t]/ {
      line = $0; sub(/^\t\t\t\t/, "", line)
      i = index(line, " = "); if (i == 0) next
      k = substr(line, 1, i - 1); gsub(/"/, "", k)
      if (k != key) next
      v = substr(line, i + 3); sub(/;$/, "", v); gsub(/"/, "", v)
      print v; exit
    }
  ' "$PROJECT"
}

app_config_ids() {
  local list
  list=$(pbx_target_config_list "$APP_TARGET")
  [ -n "$list" ] || die "target $APP_TARGET not found in $PROJECT"
  pbx_config_ids "$list" | cut -f1
}

# Distinct PROVISIONING_PROFILE_SPECIFIER values of the app target.
app_specifiers() {
  local id
  for id in $(app_config_ids); do pbx_setting "$id" PROVISIONING_PROFILE_SPECIFIER; done | awk 'NF && !seen[$0]++'
}

# pre-switch | enabled | unexpected: <why>
project_state() {
  local id base sim pre=0 enabled=0 total=0
  for id in $(app_config_ids); do
    total=$((total + 1))
    base=$(pbx_setting "$id" CODE_SIGN_ENTITLEMENTS)
    sim=$(pbx_setting "$id" "$SIMULATOR_KEY")
    if [ "$base" = "$BASE_ENTITLEMENTS" ] && [ "$sim" = "$PUSH_ENTITLEMENTS" ]; then
      pre=$((pre + 1))
    elif [ "$base" = "$PUSH_ENTITLEMENTS" ] && [ -z "$sim" ]; then
      enabled=$((enabled + 1))
    else
      echo "unexpected: configuration $id has CODE_SIGN_ENTITLEMENTS = ${base:-<unset>} and $SIMULATOR_KEY = ${sim:-<unset>}"
      return
    fi
  done
  if [ "$total" -eq 0 ]; then echo "unexpected: $APP_TARGET has no build configurations"
  elif [ "$pre" -eq "$total" ]; then echo "pre-switch"
  elif [ "$enabled" -eq "$total" ]; then echo "enabled"
  else echo "unexpected: $pre configuration(s) pre-switch and $enabled enabled"
  fi
}

# pre-switch | enabled | unexpected: <why>, from the pinning Swift test.
pinning_state() {
  [ -f "$PINNING_TEST" ] || { echo "unexpected: $PINNING_TEST is missing"; return; }
  local old new
  old=$(grep -c "func $OLD_PIN()" "$PINNING_TEST" || true)
  new=$(grep -c "func $NEW_PIN()" "$PINNING_TEST" || true)
  if [ "$old" = 1 ] && [ "$new" = 0 ]; then echo "pre-switch"
  elif [ "$old" = 0 ] && [ "$new" = 1 ]; then echo "enabled"
  else echo "unexpected: pinning test defines $OLD_PIN $old time(s) and $NEW_PIN $new time(s)"
  fi
}

# ------------------------------------------------------------------ profiles

# A profile is either a CMS envelope (installed by Xcode) or, for the fixture
# tests, a bare XML plist. Writes the plist to $2.
decode_profile() { # file out
  if head -c 5 "$1" | grep -q '^<?xml'; then
    cp "$1" "$2"
  else
    security cms -D -i "$1" -o "$2" 2>/dev/null
  fi
}

plist_value() { # file keypath -> value or empty
  plutil -extract "$2" raw -o - "$1" 2>/dev/null || true
}

plist_array() { # file keypath -> space-joined elements
  local i=0 v out=""
  while v=$(plutil -extract "$2.$i" raw -o - "$1" 2>/dev/null); do
    out="$out${out:+ }$v"
    i=$((i + 1))
  done
  printf '%s' "$out"
}

# Epoch seconds, or nothing when the value is not a date: an expiry that
# cannot be read is reported as unparsable, never as expired.
iso_to_epoch() { # 2027-09-26T04:17:56Z
  date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s 2>/dev/null || date -u -d "$1" +%s 2>/dev/null || true
}

# Index of every installed profile, one tab-separated line:
# name  uuid  team  app-id  expiry  aps  path
INDEX="$work/index.tsv"
# Profiles that could not be decoded, one path per line. Their name is unknown,
# so they could be the one Xcode picks: each is a blocker, never skipped.
UNREADABLE="$work/unreadable.txt"
build_index() {
  : >"$INDEX"
  : >"$UNREADABLE"
  local dir f plist name uuid team appid expiry aps
  local IFS=:
  for dir in $PROFILE_DIRS; do
    unset IFS
    [ -d "$dir" ] || continue
    for f in "$dir"/*.mobileprovision "$dir"/*.provisionprofile; do
      [ -f "$f" ] || continue
      plist="$work/profile.plist"
      if ! decode_profile "$f" "$plist"; then
        echo "$f" >>"$UNREADABLE"
        continue
      fi
      name=$(plist_value "$plist" Name)
      uuid=$(plist_value "$plist" UUID)
      team=$(plist_value "$plist" TeamIdentifier.0)
      appid=$(plist_value "$plist" Entitlements.application-identifier)
      expiry=$(plist_value "$plist" ExpirationDate)
      aps=$(plist_value "$plist" Entitlements.aps-environment)
      printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "$uuid" "$team" "$appid" "$expiry" "${aps:-none}" "$f" >>"$INDEX"
    done
  done
}

# --------------------------------------------------------------------- check

BLOCKERS=()
blocker() {
  local b
  for b in "${BLOCKERS[@]+"${BLOCKERS[@]}"}"; do [ "$b" = "$1" ] && return; done
  BLOCKERS+=("$1")
}

days_left() { echo $(( ($1 - NOW) / 86400 )); }

# The valid codesigning identities in the keychain, by name: one per line in
# $IDENTITIES, read once. The certificate hashes `security` prints beside the
# names are dropped here and never kept or shown.
IDENTITIES="$work/identities.txt"
IDENTITIES_STATE="" # listed | failed
find_identities() {
  if [ -n "$FIND_IDENTITY" ]; then "$FIND_IDENTITY"; else security find-identity -v -p codesigning; fi
}
list_identities() {
  [ -z "$IDENTITIES_STATE" ] || return 0
  local raw
  if raw=$(find_identities 2>/dev/null); then
    printf '%s\n' "$raw" | sed -nE 's/^ *[0-9]+\) [0-9A-Fa-f]+ "(.*)"$/\1/p' >"$IDENTITIES"
    IDENTITIES_STATE=listed
  else
    : >"$IDENTITIES"
    IDENTITIES_STATE=failed
  fi
}

# A device build signs with the identity the configuration pins; the
# certificate and its private key must be in the keychain and still valid, or
# the build fails after everything else is ready.
check_identity() { # config-id
  local id=$1 identity
  identity=$(pbx_setting "$id" 'CODE_SIGN_IDENTITY[sdk=iphoneos*]')
  [ -n "$identity" ] || identity=$(pbx_setting "$id" CODE_SIGN_IDENTITY)
  if [ -z "$identity" ]; then
    echo "    signing identity: not pinned (Xcode picks a matching certificate; nothing to verify here)"
    return
  fi
  list_identities
  if [ "$IDENTITIES_STATE" = failed ]; then
    echo "    signing identity: \"$identity\" -> BLOCKED (the keychain could not be listed)"
    blocker "could not list codesigning identities (security find-identity -v -p codesigning failed), so the signing identity \"$identity\" is unverified"
  elif grep -qxF -- "$identity" "$IDENTITIES"; then
    echo "    signing identity: \"$identity\" -> ok (present and valid in the keychain)"
  else
    echo "    signing identity: \"$identity\" -> BLOCKED"
    blocker "signing identity \"$identity\" is not present and valid in the keychain (security find-identity -v -p codesigning lists none by that name); install or renew the certificate with its private key; no device build signs without it"
  fi
}

# Reports every installed profile named like the configuration's specifier and
# records blockers. $4 says whether aps-environment is required.
check_target() { # target requires-aps(yes|no)
  local target=$1 requires=$2 list id name spec bundle team style ents sim
  list=$(pbx_target_config_list "$target")
  [ -n "$list" ] || die "target $target not found in $PROJECT"
  echo "target $target"
  while IFS=$'\t' read -r id name; do
    spec=$(pbx_setting "$id" PROVISIONING_PROFILE_SPECIFIER)
    bundle=$(pbx_setting "$id" PRODUCT_BUNDLE_IDENTIFIER)
    team=$(pbx_setting "$id" DEVELOPMENT_TEAM)
    style=$(pbx_setting "$id" CODE_SIGN_STYLE)
    ents=$(pbx_setting "$id" CODE_SIGN_ENTITLEMENTS)
    sim=$(pbx_setting "$id" "$SIMULATOR_KEY")
    echo "  $name: bundle $bundle, team $team, $style signing, profile \"$spec\""
    echo "    entitlements: device ${ents:-<unset>}, simulator ${sim:-$ents}"
    if [ "$style" != Manual ] || [ -z "$spec" ]; then
      blocker "configuration $name of $target uses ${style:-unknown} signing without a profile specifier; this script checks manual profiles"
      continue
    fi
    check_identity "$id"
    if [ "$requires" = yes ]; then
      echo "    aps-environment: required ($REQUIRED_APS; ActivityKit push tokens belong to the app)"
    else
      echo "    aps-environment: not required (ActivityKit push tokens belong to the app, not the widget)"
    fi
    local found=0 p_name p_uuid p_team p_appid p_expiry p_aps p_path exp_epoch left verdict
    while IFS=$'\t' read -r p_name p_uuid p_team p_appid p_expiry p_aps p_path; do
      [ "$p_name" = "$spec" ] || continue
      found=$((found + 1))
      verdict=ok
      exp_epoch=$(iso_to_epoch "$p_expiry")
      if [ -n "$exp_epoch" ]; then left="$(days_left "$exp_epoch") days"; else left=unparsable; fi
      if [ "$p_team" != "$team" ]; then
        blocker "profile \"$spec\" ($p_uuid) is for team $p_team, not $team"; verdict=BLOCKED
      fi
      # The App ID prefix is assumed to equal DEVELOPMENT_TEAM. That holds for
      # this project (one team, no legacy prefix) and keeps the match exact:
      # a profile for another prefix is reported, not accepted on a suffix.
      if [ "$p_appid" != "$team.$bundle" ] && [ "$p_appid" != "$team.*" ]; then
        blocker "profile \"$spec\" ($p_uuid) is for app ID $p_appid, not $team.$bundle"; verdict=BLOCKED
      fi
      if [ -z "$exp_epoch" ]; then
        blocker "profile \"$spec\" ($p_uuid) has an unparsable expiry \"$p_expiry\"; remove it and install a current one"; verdict=BLOCKED
      elif [ "$exp_epoch" -le "$NOW" ]; then
        blocker "profile \"$spec\" ($p_uuid) expired on $p_expiry; remove it and install a current one"; verdict=BLOCKED
      fi
      if [ "$requires" = yes ]; then
        if [ "$p_aps" = none ]; then
          blocker "profile \"$spec\" ($p_uuid) lacks aps-environment"; verdict=BLOCKED
        elif [ "$p_aps" != "$REQUIRED_APS" ]; then
          blocker "profile \"$spec\" ($p_uuid) has aps-environment $p_aps; $PUSH_ENTITLEMENTS asks for $REQUIRED_APS"; verdict=BLOCKED
        fi
      fi
      echo "    profile \"$p_name\" ($p_uuid): team $p_team, app ID $p_appid, expires $p_expiry ($left), aps-environment: $p_aps -> $verdict"
      echo "      $p_path"
    done <"$INDEX"
    if [ "$found" -eq 0 ]; then
      blocker "no installed profile named \"$spec\" (looked in: ${PROFILE_DIRS//:/, })"
      echo "    no installed profile named \"$spec\""
    elif [ "$found" -gt 1 ]; then
      echo "    note: $found profiles share this name; Xcode picks one of them, so every one must be ready"
    fi
  done < <(pbx_config_ids "$list")
}

# The app's resolver, from the outside: simulator -> sandbox; no embedded
# profile on a device build -> production (distribution); otherwise the
# embedded profile's aps-environment decides, anything else is nil.
check_app() { # path
  local app=$1 info platform ents aps groups keychain embedded plist e_name e_uuid e_expiry e_aps resolver widget w_ents w_aps
  [ -d "$app" ] && [ -f "$app/Info.plist" ] || die "$app is not an app bundle"
  info="$app/Info.plist"
  platform=$(plist_value "$info" DTPlatformName)
  ents="$work/app-entitlements.plist"
  "$CODESIGN" -d --entitlements - --xml "$app" >"$ents" 2>/dev/null || die "codesign could not read entitlements of $app"
  aps=$(plist_value "$ents" aps-environment)
  groups=$(plist_array "$ents" 'com\.apple\.security\.application-groups')
  keychain=$(plist_array "$ents" 'keychain-access-groups')
  echo "app $app"
  echo "  platform: ${platform:-unknown}"
  echo "  codesign aps-environment: ${aps:-none}"
  echo "  codesign application-groups: ${groups:-none}"
  echo "  codesign keychain-access-groups: ${keychain:-none}"
  embedded="$app/embedded.mobileprovision"
  if [ "$platform" = iphonesimulator ]; then
    echo "  embedded profile: none (simulator build)"
    resolver="sandbox (simulator branch)"
  elif [ ! -f "$embedded" ]; then
    echo "  embedded profile: none (App Store/TestFlight distribution)"
    resolver="production (no embedded profile)"
    [ -n "$aps" ] || blocker "$app is signed without aps-environment; push stays inert on this build"
  else
    plist="$work/embedded.plist"
    if ! decode_profile "$embedded" "$plist"; then
      echo "  embedded profile: unreadable"
      blocker "$app has an unreadable embedded.mobileprovision"
      resolver="nil (push stays inert)"
    else
      e_name=$(plist_value "$plist" Name)
      e_uuid=$(plist_value "$plist" UUID)
      e_expiry=$(plist_value "$plist" ExpirationDate)
      e_aps=$(plist_value "$plist" Entitlements.aps-environment)
      echo "  embedded profile: \"$e_name\" ($e_uuid), expires $e_expiry, aps-environment: ${e_aps:-none}"
      # Runbook D-3 expects the app target's own profile, not any profile that
      # happens to sign the bundle ID (such as an automatic team profile).
      local specs spec_ok=no s
      specs=$(app_specifiers)
      while IFS= read -r s; do [ "$s" = "$e_name" ] && spec_ok=yes; done <<<"$specs"
      if [ "$spec_ok" = no ]; then
        blocker "embedded profile \"$e_name\" ($e_uuid) is not the app target's profile \"$(printf '%s' "$specs" | paste -sd '|' -)\""
      fi
      case "${e_aps:-none}" in
        development) resolver="sandbox (embedded profile aps-environment development)" ;;
        production) resolver="production (embedded profile aps-environment production)" ;;
        *) resolver="nil (push stays inert)" ;;
      esac
      if [ "${aps:-none}" != "${e_aps:-none}" ]; then
        blocker "codesign (${aps:-none}) and embedded profile (${e_aps:-none}) disagree for $app"
      fi
      [ -n "$aps" ] || blocker "$app is signed without aps-environment; push stays inert on this build"
    fi
  fi
  echo "  resolver: $resolver"
  widget="$app/PlugIns/$WIDGET_TARGET.appex"
  if [ -d "$widget" ]; then
    w_ents="$work/widget-entitlements.plist"
    if "$CODESIGN" -d --entitlements - --xml "$widget" >"$w_ents" 2>/dev/null; then
      w_aps=$(plist_value "$w_ents" aps-environment)
      if [ -z "$w_aps" ]; then
        echo "  widget aps-environment: none (expected; ActivityKit push tokens belong to the app)"
      else
        echo "  widget aps-environment: $w_aps (not required; harmless)"
      fi
    else
      echo "  widget: codesign could not read entitlements"
    fi
  else
    echo "  widget: $widget not present"
  fi
}

print_result() {
  local b
  if [ "${#BLOCKERS[@]}" -eq 0 ]; then
    echo "result: ready (every device configuration of $APP_TARGET has a push-capable profile)"
    return 0
  fi
  for b in "${BLOCKERS[@]}"; do echo "blocked: $b"; done
  echo "result: blocked (${#BLOCKERS[@]})"
  return 1
}

run_check() { # [app-path]
  echo "project: $PROJECT"
  echo "profiles: ${PROFILE_DIRS//:/, }"
  build_index
  local f
  while IFS= read -r f; do
    [ -n "$f" ] && blocker "unreadable profile at $f (security cms -D failed; its name is unknown, so it may be the one Xcode picks)"
  done <"$UNREADABLE"
  check_target "$APP_TARGET" yes
  check_target "$WIDGET_TARGET" no
  local state
  state=$(project_state)
  case "$state" in
    pre-switch) echo "project state: pre-switch (device builds sign with $BASE_ENTITLEMENTS, no aps-environment; run \`enable\` after P1)" ;;
    enabled) echo "project state: enabled (device builds sign with $PUSH_ENTITLEMENTS)" ;;
    *) echo "project state: $state"; blocker "$state" ;;
  esac
  if [ -n "${1:-}" ]; then check_app "$1"; fi
}

# -------------------------------------------------------------------- enable

rewrite_project() { # in out
  local ids
  ids=$(app_config_ids | tr '\n' ' ')
  awk -v ids=" $ids" -v base="$BASE_ENTITLEMENTS" -v push="$PUSH_ENTITLEMENTS" -v simkey="$SIMULATOR_KEY" '
    /^\t\t[A-F0-9]+ \/\* .* \*\/ = \{$/ { cur = $1 }
    /^\t\t\};$/ { cur = "" }
    cur != "" && index(ids, " " cur " ") {
      if ($0 == "\t\t\t\tCODE_SIGN_ENTITLEMENTS = " base ";") { print "\t\t\t\tCODE_SIGN_ENTITLEMENTS = " push ";"; next }
      if ($0 == "\t\t\t\t\"" simkey "\" = " push ";") next
    }
    { print }
  ' "$1" >"$2"
}

new_pinning_test() {
  cat <<'EOF'
    @Test func pushEntitlementsSignEveryAppConfiguration() throws {
        let project = try String(contentsOf: Self.repository.appending(path: "BabyTracker.xcodeproj/project.pbxproj"), encoding: .utf8)
        // Debug and Release of the app target sign with the push file on every
        // SDK (runbook P2, applied by scripts/live-activity-push-signing.sh
        // enable); the simulator-only conditional is gone with it.
        #expect(project.components(separatedBy: "CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;").count - 1 == 2)
        #expect(!project.contains("CODE_SIGN_ENTITLEMENTS = Apps/BabyTracker.entitlements;"))
        #expect(!project.contains("\"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]\""))
    }
EOF
}

rewrite_pinning_test() { # in out
  BTLAP_NEW_TEST=$(new_pinning_test; printf x)
  BTLAP_NEW_TEST=${BTLAP_NEW_TEST%x}
  export BTLAP_NEW_TEST
  perl -0777 -pe 's/^    \@Test func '"$OLD_PIN"'\(\) throws \{\n.*?\n    \}\n/$ENV{BTLAP_NEW_TEST}/sm' "$1" >"$2"
  unset BTLAP_NEW_TEST
  cmp -s "$1" "$2" && die "could not locate $OLD_PIN in $PINNING_TEST"
  grep -q "func $NEW_PIN()" "$2" || die "rewrite of $PINNING_TEST did not produce $NEW_PIN"
}

relative() { # path -> relative to the project root when inside it
  case "$1" in
    "$project_root"/*) printf '%s' "${1#"$project_root"/}" ;;
    *) printf '%s' "$1" ;;
  esac
}

follow_up() {
  cat <<EOF
next (runbook P2), from $project_root:
  1. TMPDIR=\$(mktemp -d /tmp/btlap.XXXXXX) make test
       # EXPECTED_TESTS unchanged: one pinning test replaced another
  2. make build SIMULATOR='$SIMULATOR'
  3. xcodebuild -scheme $APP_TARGET -configuration Debug -destination 'generic/platform=iOS' \\
       -derivedDataPath /private/tmp/bt-lap-phone-build -quiet build
  4. scripts/live-activity-push-signing.sh check --app /private/tmp/bt-lap-phone-build/Build/Products/Debug-iphoneos/$APP_TARGET.app
  5. git add $(relative "$PROJECT") $(relative "$PINNING_TEST") && git commit   # one reviewed commit
EOF
}

run_enable() { # dry-run(yes|no)
  local dry=$1 state pin ready=yes
  run_check
  print_result || ready=no
  state=$(project_state)
  pin=$(pinning_state)
  case "$state" in
    enabled)
      if [ "$pin" != enabled ]; then
        echo "refused: project is enabled but the pinning test is $pin"
        return 1
      fi
      if [ "$ready" = no ]; then
        echo "already enabled, but check is blocked: see the blocked: lines above; a device build will not sign until they are fixed"
        return 1
      fi
      echo "already enabled: nothing to write"
      return 0
      ;;
    pre-switch) ;;
    *)
      echo "refused: unexpected project state: ${state#unexpected: }"
      return 1
      ;;
  esac
  if [ "$pin" != pre-switch ]; then
    echo "refused: project is pre-switch but the pinning test is $pin"
    return 1
  fi
  if [ "$dry" = no ] && [ "$ready" = no ]; then
    echo "refused: not every device configuration of $APP_TARGET is ready (see the blocked: lines); nothing written"
    return 1
  fi
  rewrite_project "$PROJECT" "$work/project.pbxproj"
  rewrite_pinning_test "$PINNING_TEST" "$work/pinning.swift"
  local changed
  changed=$(grep -c "CODE_SIGN_ENTITLEMENTS = $PUSH_ENTITLEMENTS;" "$work/project.pbxproj" || true)
  if [ "$dry" = yes ]; then
    diff -u --label "$PROJECT" --label "$PROJECT (after enable)" "$PROJECT" "$work/project.pbxproj" || true
    diff -u --label "$PINNING_TEST" --label "$PINNING_TEST (after enable)" "$PINNING_TEST" "$work/pinning.swift" || true
    if [ "$ready" = no ]; then
      echo "dry-run: enable would be refused (not every device configuration of $APP_TARGET is ready); nothing written"
      return 1
    fi
    echo "dry-run: nothing written; enable would apply the diff above"
    return 0
  fi
  cat "$work/project.pbxproj" >"$PROJECT"
  cat "$work/pinning.swift" >"$PINNING_TEST"
  echo "enabled: $changed configuration(s) of $APP_TARGET now sign with $PUSH_ENTITLEMENTS; the simulator-only conditional is removed"
  echo "pinning test: $OLD_PIN -> $NEW_PIN in $PINNING_TEST"
  follow_up
}

# ---------------------------------------------------------------------- main

[ -f "$PROJECT" ] || die "project file not found: $PROJECT"
mode=${1:-}
[ -n "$mode" ] || usage
shift

case "$mode" in
  check)
    app=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --app) [ $# -ge 2 ] || usage; app=$2; shift 2 ;;
        *) usage ;;
      esac
    done
    run_check "$app"
    print_result
    ;;
  enable)
    dry=no
    while [ $# -gt 0 ]; do
      case "$1" in
        --dry-run) dry=yes; shift ;;
        *) usage ;;
      esac
    done
    run_enable "$dry"
    ;;
  state)
    [ $# -eq 0 ] || usage
    state=$(project_state)
    pin=$(pinning_state)
    echo "project: $state"
    echo "pinning test: $pin"
    case "$state" in unexpected*) exit 1 ;; esac
    case "$pin" in unexpected*) exit 1 ;; esac
    [ "$state" = "$pin" ] || { echo "mismatch: project and pinning test disagree"; exit 1; }
    ;;
  *)
    usage
    ;;
esac
