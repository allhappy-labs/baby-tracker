#!/usr/bin/env bash
# Fixture tests for scripts/live-activity-push-signing.sh.
#
# Everything runs against a temporary directory: synthetic provisioning
# profiles (plain XML plists, no certificates), a snapshot of project.pbxproj
# and of the pinning Swift test from scripts/testdata, and a stub `codesign`.
# The real profiles, project file and Swift tests are never written. The last
# case reads the real project file once, to classify its state.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/live-activity-push-signing.sh"
testdata="$here/testdata/live-activity-push-signing"
repo=$(cd "$here/.." && pwd)

pass=0
fail=0
current=""
case_fail=0

begin() {
  current=$1
  case_fail=0
}

fail_case() {
  case_fail=$((case_fail + 1))
  echo "FAIL [$current]: $1"
  if [ -n "${2:-}" ]; then
    echo "----- output -----"
    echo "$2"
    echo "------------------"
  fi
}

finish() {
  if [ "$case_fail" -eq 0 ]; then
    pass=$((pass + 1))
    echo "ok   [$current]"
  else
    fail=$((fail + 1))
  fi
}

expect_status() { # want got output
  [ "$1" = "$2" ] || fail_case "exit status $2, want $1" "$3"
}

expect_contains() { # output needle
  grep -qF -- "$2" <<<"$1" || fail_case "output lacks: $2" "$1"
}

expect_not_contains() { # output needle
  if grep -qF -- "$2" <<<"$1"; then fail_case "output must not contain: $2" "$1"; fi
}

expect_unchanged() { # path checksum-before what
  [ "$(checksum "$1")" = "$2" ] || fail_case "$3 changed"
}

# A synthetic profile. $8 (aps) may be empty for "no aps-environment".
write_profile() { # dir file name uuid team appid expiry [aps]
  local dir=$1 file=$2 name=$3 uuid=$4 team=$5 appid=$6 expiry=$7 aps=${8:-}
  local aps_xml=""
  if [ -n "$aps" ]; then
    aps_xml="<key>aps-environment</key><string>$aps</string>"
  fi
  cat >"$dir/$file" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Name</key><string>$name</string>
	<key>UUID</key><string>$uuid</string>
	<key>TeamIdentifier</key><array><string>$team</string></array>
	<key>TeamName</key><string>Fixture Team</string>
	<key>CreationDate</key><date>2026-09-24T10:00:00Z</date>
	<key>ExpirationDate</key><date>$expiry</date>
	<key>Entitlements</key>
	<dict>
		<key>application-identifier</key><string>$appid</string>
		$aps_xml
		<key>com.apple.security.application-groups</key><array><string>group.com.example.babytracker</string></array>
	</dict>
</dict>
</plist>
EOF
}

APP_NAME="BabyTracker Development - TEAM123456"
WIDGET_NAME="BabyTrackerWidgets Development - TEAM123456"
TEAM=TEAM123456
APP_ID="$TEAM.com.example.babytracker"
WIDGET_ID="$TEAM.com.example.babytracker.widgets"
FUTURE=2027-09-26T04:17:56Z
PAST=2026-01-01T00:00:00Z
NOW=1790000000 # 2026-09-21T12:53:20Z: after PAST, before FUTURE
# The identity the fixture project pins for device builds, and a hash the
# stub listing attaches to every identity: the script must never repeat it.
IDENTITY="Apple Development: developer@example.com (CERT123456)"
FAKE_HASH=0123456789ABCDEF0123456789ABCDEF01234567

# A stub for `security find-identity -v -p codesigning`: lists the given
# names as valid identities, in that command's format.
install_identity_stub() { # names...
  {
    echo '#!/usr/bin/env bash'
    echo 'cat <<"LIST"'
    local i=0 n
    for n in "$@"; do i=$((i + 1)); echo "  $i) $FAKE_HASH \"$n\""; done
    echo "     $i valid identities found"
    echo 'LIST'
  } >"$sandbox/find-identity"
  chmod +x "$sandbox/find-identity"
  export BTLAP_FIND_IDENTITY="$sandbox/find-identity"
}

# Fresh sandbox: two profile dirs, a project copy, a Swift copy, and a keychain
# listing that holds the pinned identity. Exports the environment the script
# reads.
setup() {
  sandbox=$(mktemp -d "${TMPDIR:-/tmp}/btlap-signing.XXXXXX")
  mkdir -p "$sandbox/profiles-a" "$sandbox/profiles-b" "$sandbox/Packages/BabyTracker/Tests/BabySyncTests" "$sandbox/BabyTracker.xcodeproj"
  cp "$testdata/project.pbxproj" "$sandbox/BabyTracker.xcodeproj/project.pbxproj"
  cp "$testdata/LiveActivityPushEligibilityTests.swift" "$sandbox/Packages/BabyTracker/Tests/BabySyncTests/"
  export BTLAP_PROFILE_DIRS="$sandbox/profiles-a:$sandbox/profiles-b"
  export BTLAP_PROJECT="$sandbox/BabyTracker.xcodeproj/project.pbxproj"
  export BTLAP_PINNING_TEST="$sandbox/Packages/BabyTracker/Tests/BabySyncTests/LiveActivityPushEligibilityTests.swift"
  export BTLAP_NOW="$NOW"
  unset BTLAP_CODESIGN BTLAP_SIMULATOR
  install_identity_stub "Apple Development: Someone Else (FG6YU6TMR6)" "$IDENTITY"
}

ready_profiles() {
  write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" "$FUTURE" development
  write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
}

# App profile without aps-environment, widget profile fine: today's real state.
blocked_profiles() {
  write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" "$FUTURE"
  write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
}

checksum() { shasum -a 256 "$1" | cut -d' ' -f1; }

run() { # args...; sets out and status
  out=$("$script" "$@" 2>&1)
  status=$?
}

# A stub codesign: prints the plist stored next to the bundle it is asked about.
install_codesign_stub() {
  cat >"$sandbox/codesign" <<'EOF'
#!/usr/bin/env bash
# Mimics `codesign -d --entitlements - --xml <path>`: the path is the last argument.
for last; do :; done
cat "$last/_entitlements.plist"
EOF
  chmod +x "$sandbox/codesign"
  export BTLAP_CODESIGN="$sandbox/codesign"
}

write_app() { # dir platform [aps]; writes Info.plist and the stub entitlements
  local app=$1 platform=$2 aps=${3:-}
  mkdir -p "$app/PlugIns/BabyTrackerWidgets.appex"
  cat >"$app/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.babytracker</string>
<key>DTPlatformName</key><string>$platform</string>
</dict></plist>
EOF
  local aps_xml=""
  [ -n "$aps" ] && aps_xml="<key>aps-environment</key><string>$aps</string>"
  cat >"$app/_entitlements.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
$aps_xml
<key>application-identifier</key><string>$APP_ID</string>
<key>com.apple.security.application-groups</key><array><string>group.com.example.babytracker</string></array>
<key>keychain-access-groups</key><array><string>$TEAM.group.com.example.babytracker</string></array>
</dict></plist>
EOF
  cat >"$app/PlugIns/BabyTrackerWidgets.appex/_entitlements.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>application-identifier</key><string>$WIDGET_ID</string>
<key>com.apple.security.application-groups</key><array><string>group.com.example.babytracker</string></array>
</dict></plist>
EOF
}

# ---------------------------------------------------------------- check

begin "check: ready"
setup; ready_profiles
run check
expect_status 0 "$status" "$out"
expect_contains "$out" "result: ready"
expect_contains "$out" "11111111-1111-1111-1111-111111111111"
expect_contains "$out" "aps-environment: development"
expect_contains "$out" "22222222-2222-2222-2222-222222222222"
expect_contains "$out" "not required"
expect_contains "$out" "project state: pre-switch"
expect_contains "$out" "signing identity: \"$IDENTITY\" -> ok"
expect_not_contains "$out" "$FAKE_HASH"
expect_not_contains "$out" "blocked:"
finish

begin "check: pinned signing identity missing from the keychain"
setup; ready_profiles
install_identity_stub "Apple Development: Someone Else (FG6YU6TMR6)" "Apple Distribution: Example Developer (TEAM123456)"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: signing identity \"$IDENTITY\" is not present and valid in the keychain"
expect_contains "$out" "result: blocked (1)"
expect_not_contains "$out" "$FAKE_HASH"
finish

begin "check: identity listing fails"
setup; ready_profiles
printf '#!/usr/bin/env bash\necho "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2\nexit 1\n' >"$sandbox/find-identity"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: could not list codesigning identities"
expect_contains "$out" "\"$IDENTITY\" is unverified"
finish

begin "check: unparsable profile expiry is reported as such, not as expired"
setup
write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" someday development
perl -pi -e 's|<date>someday</date>|<string>someday</string>|' "$sandbox/profiles-a/app.mobileprovision"
write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: profile \"$APP_NAME\" (11111111-1111-1111-1111-111111111111) has an unparsable expiry \"someday\""
expect_not_contains "$out" "expired on"
expect_contains "$out" "expires someday (unparsable)"
finish

begin "check: app profile lacks aps-environment"
setup; blocked_profiles
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: profile \"$APP_NAME\" (11111111-1111-1111-1111-111111111111) lacks aps-environment"
expect_contains "$out" "result: blocked"
finish

begin "check: expired app profile"
setup
write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" "$PAST" development
write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: profile \"$APP_NAME\" (11111111-1111-1111-1111-111111111111) expired"
finish

begin "check: no installed profile for the app"
setup
write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: no installed profile named \"$APP_NAME\""
finish

begin "check: widget profile missing is a blocker too"
setup
write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" "$FUTURE" development
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: no installed profile named \"$WIDGET_NAME\""
finish

begin "check: stale duplicate without aps-environment blocks even next to a good one"
setup; ready_profiles
write_profile "$sandbox/profiles-b" stale.mobileprovision "$APP_NAME" 33333333-3333-3333-3333-333333333333 "$TEAM" "$APP_ID" "$FUTURE"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: profile \"$APP_NAME\" (33333333-3333-3333-3333-333333333333) lacks aps-environment"
expect_contains "$out" "11111111-1111-1111-1111-111111111111"
finish

begin "check: profile for another app ID is not a match"
setup
write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$TEAM.com.example.other" "$FUTURE" development
write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: profile \"$APP_NAME\" (11111111-1111-1111-1111-111111111111) is for app ID $TEAM.com.example.other, not $APP_ID"
finish

begin "check: a profile with the wrong aps-environment value is not ready"
setup
write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" "$FUTURE" production
write_profile "$sandbox/profiles-b" widget.mobileprovision "$WIDGET_NAME" 22222222-2222-2222-2222-222222222222 "$TEAM" "$WIDGET_ID" "$FUTURE"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "has aps-environment production; Apps/BabyTrackerLiveActivityPush.entitlements asks for development"
finish

begin "check: an undecodable profile in the directories is a blocker, not skipped"
setup; ready_profiles
printf '\x30\x82\x01\x00garbage-not-cms-not-xml' >"$sandbox/profiles-b/broken.mobileprovision"
run check
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: unreadable profile at $sandbox/profiles-b/broken.mobileprovision"
finish

begin "check: output never carries certificate material"
setup; ready_profiles
# A fake certificate entry in the app profile; the report must not echo it.
perl -0777 -pi -e 's/(\t<key>Name<\/key>)/\t<key>DeveloperCertificates<\/key><array><data>MIIFAKECERT<\/data><\/array>\n$1/' "$sandbox/profiles-a/app.mobileprovision"
run check
expect_status 0 "$status" "$out"
expect_not_contains "$out" "MIIFAKECERT"
finish

# ---------------------------------------------------------------- check --app

begin "check --app: signed device build with a development profile resolves sandbox"
setup; ready_profiles; install_codesign_stub
write_app "$sandbox/BabyTracker.app" iphoneos development
cp "$sandbox/profiles-a/app.mobileprovision" "$sandbox/BabyTracker.app/embedded.mobileprovision"
run check --app "$sandbox/BabyTracker.app"
expect_status 0 "$status" "$out"
expect_contains "$out" "codesign aps-environment: development"
expect_contains "$out" "application-groups: group.com.example.babytracker"
expect_contains "$out" "keychain-access-groups: $TEAM.group.com.example.babytracker"
expect_contains "$out" "embedded profile: \"$APP_NAME\" (11111111-1111-1111-1111-111111111111)"
expect_contains "$out" "resolver: sandbox"
expect_contains "$out" "widget aps-environment: none (expected"
finish

begin "check --app: device build whose profile lacks aps-environment is inert and blocked"
setup; ready_profiles; install_codesign_stub
write_app "$sandbox/BabyTracker.app" iphoneos
write_profile "$sandbox/BabyTracker.app" embedded.mobileprovision "$APP_NAME" 44444444-4444-4444-4444-444444444444 "$TEAM" "$APP_ID" "$FUTURE"
run check --app "$sandbox/BabyTracker.app"
expect_status 1 "$status" "$out"
expect_contains "$out" "codesign aps-environment: none"
expect_contains "$out" "resolver: nil (push stays inert)"
expect_contains "$out" "blocked: $sandbox/BabyTracker.app is signed without aps-environment"
finish

begin "check --app: codesign and embedded profile disagreeing is a blocker"
setup; ready_profiles; install_codesign_stub
write_app "$sandbox/BabyTracker.app" iphoneos development
write_profile "$sandbox/BabyTracker.app" embedded.mobileprovision "$APP_NAME" 44444444-4444-4444-4444-444444444444 "$TEAM" "$APP_ID" "$FUTURE"
run check --app "$sandbox/BabyTracker.app"
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: codesign (development) and embedded profile (none) disagree"
finish

begin "check --app: an embedded profile that is not the app target's specifier is a blocker"
setup; ready_profiles; install_codesign_stub
write_app "$sandbox/BabyTracker.app" iphoneos development
write_profile "$sandbox/BabyTracker.app" embedded.mobileprovision "iOS Team Provisioning Profile: com.example.babytracker" 55555555-5555-5555-5555-555555555555 "$TEAM" "$APP_ID" "$FUTURE" development
run check --app "$sandbox/BabyTracker.app"
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: embedded profile \"iOS Team Provisioning Profile: com.example.babytracker\" (55555555-5555-5555-5555-555555555555) is not the app target's profile \"$APP_NAME\""
finish

begin "check --app: simulator build has no profile and resolves sandbox"
setup; ready_profiles; install_codesign_stub
write_app "$sandbox/BabyTracker.app" iphonesimulator development
run check --app "$sandbox/BabyTracker.app"
expect_status 0 "$status" "$out"
expect_contains "$out" "embedded profile: none (simulator build)"
expect_contains "$out" "resolver: sandbox"
finish

begin "check --app: device build without an embedded profile is distribution, production"
setup; ready_profiles; install_codesign_stub
write_app "$sandbox/BabyTracker.app" iphoneos production
run check --app "$sandbox/BabyTracker.app"
expect_status 0 "$status" "$out"
expect_contains "$out" "embedded profile: none (App Store/TestFlight distribution)"
expect_contains "$out" "resolver: production"
finish

begin "check --app: a path that is not an app bundle is a usage error"
setup; ready_profiles
run check --app "$sandbox/nope.app"
expect_status 2 "$status" "$out"
finish

# ---------------------------------------------------------------- enable

begin "enable --dry-run: prints the diff and writes nothing"
setup; ready_profiles
before_project=$(checksum "$BTLAP_PROJECT"); before_test=$(checksum "$BTLAP_PINNING_TEST")
run enable --dry-run
expect_status 0 "$status" "$out"
expect_contains "$out" "-				CODE_SIGN_ENTITLEMENTS = Apps/BabyTracker.entitlements;"
expect_contains "$out" "+				CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;"
expect_contains "$out" "-				\"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]\" = Apps/BabyTrackerLiveActivityPush.entitlements;"
expect_contains "$out" "-    @Test func pushEntitlementsAreSelectedOnlyForTheSimulatorSDK() throws {"
expect_contains "$out" "+    @Test func pushEntitlementsSignEveryAppConfiguration() throws {"
expect_contains "$out" "dry-run: nothing written"
expect_unchanged "$BTLAP_PROJECT" "$before_project" "project file (dry-run)"
expect_unchanged "$BTLAP_PINNING_TEST" "$before_test" "pinning test (dry-run)"
finish

begin "enable --dry-run while blocked: shows the diff but states the refusal"
setup; blocked_profiles
before_project=$(checksum "$BTLAP_PROJECT"); before_test=$(checksum "$BTLAP_PINNING_TEST")
run enable --dry-run
expect_status 1 "$status" "$out"
expect_contains "$out" "+				CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;"
expect_contains "$out" "dry-run: enable would be refused"
expect_contains "$out" "lacks aps-environment"
expect_unchanged "$BTLAP_PROJECT" "$before_project" "project file (refused dry-run)"
expect_unchanged "$BTLAP_PINNING_TEST" "$before_test" "pinning test (refused dry-run)"
finish

begin "enable: refused while blocked, nothing written"
setup; blocked_profiles
before_project=$(checksum "$BTLAP_PROJECT"); before_test=$(checksum "$BTLAP_PINNING_TEST")
run enable
expect_status 1 "$status" "$out"
expect_contains "$out" "refused: not every device configuration of BabyTracker is ready"
expect_not_contains "$out" "+				CODE_SIGN_ENTITLEMENTS"
expect_unchanged "$BTLAP_PROJECT" "$before_project" "project file (refused enable)"
expect_unchanged "$BTLAP_PINNING_TEST" "$before_test" "pinning test (refused enable)"
finish

begin "enable: applies the switch and updates the pinning test"
setup; ready_profiles
run enable
expect_status 0 "$status" "$out"
expect_contains "$out" "enabled: 2 configuration(s) of BabyTracker now sign with Apps/BabyTrackerLiveActivityPush.entitlements"
expect_contains "$out" "make test"
expect_contains "$out" "platform=iOS Simulator,name=iPhone 17 Pro"
expect_contains "$out" "check --app"
project_after=$(cat "$BTLAP_PROJECT")
swift_after=$(cat "$BTLAP_PINNING_TEST")
[ "$(grep -cF 'CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;' <<<"$project_after")" = 2 ] || fail_case "push entitlements not on both app configurations"
if grep -qF 'CODE_SIGN_ENTITLEMENTS = Apps/BabyTracker.entitlements;' <<<"$project_after"; then fail_case "base entitlements still referenced"; fi
if grep -qF 'CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]' <<<"$project_after"; then fail_case "simulator conditional still present"; fi
[ "$(grep -cF 'CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerWidgets.entitlements;' <<<"$project_after")" = 2 ] || fail_case "widget entitlements changed"
# Only the two app configurations changed: line count drops by exactly the two removed conditionals.
[ $(( $(wc -l <"$testdata/project.pbxproj") - $(wc -l <"$BTLAP_PROJECT") )) -eq 2 ] || fail_case "unexpected line count change in project file"
grep -qF 'func pushEntitlementsSignEveryAppConfiguration()' <<<"$swift_after" || fail_case "new pinning test missing"
if grep -qF 'func pushEntitlementsAreSelectedOnlyForTheSimulatorSDK()' <<<"$swift_after"; then fail_case "old pinning test still present"; fi
grep -qF 'func pushEntitlementsAreExactlyTheAppEntitlementsPlusTheDevelopmentGateway()' <<<"$swift_after" || fail_case "neighbouring test lost"
grep -qF 'func startAlertKeysAreLocalizedInTheAppBundleCatalogForEveryLocale()' <<<"$swift_after" || fail_case "neighbouring test lost"
[ "$(grep -c '@Test func' <<<"$swift_after")" = "$(grep -c '@Test func' <"$testdata/LiveActivityPushEligibilityTests.swift")" ] || fail_case "test count changed"
# The new pin asserts exactly the state the script produced.
grep -qF 'project.components(separatedBy: "CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;").count - 1 == 2' <<<"$swift_after" || fail_case "new pin does not count the push lines"
grep -qF '!project.contains("CODE_SIGN_ENTITLEMENTS = Apps/BabyTracker.entitlements;")' <<<"$swift_after" || fail_case "new pin does not exclude the base file"
grep -qF '!project.contains("\"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]\"")' <<<"$swift_after" || fail_case "new pin does not exclude the simulator conditional"
run state
expect_status 0 "$status" "$out"
expect_contains "$out" "enabled"
finish

begin "enable: idempotent re-run changes nothing"
# continues in the sandbox of the previous case
before_project=$(checksum "$BTLAP_PROJECT"); before_test=$(checksum "$BTLAP_PINNING_TEST")
run enable
expect_status 0 "$status" "$out"
expect_contains "$out" "already enabled"
expect_unchanged "$BTLAP_PROJECT" "$before_project" "project file (idempotent enable)"
expect_unchanged "$BTLAP_PINNING_TEST" "$before_test" "pinning test (idempotent enable)"
run check
expect_status 0 "$status" "$out"
expect_contains "$out" "project state: enabled"
finish

begin "enable: already enabled but blocked points at the blocked: lines and exits 1"
# continues in the enabled sandbox; swap the app profile for one without aps-environment
write_profile "$sandbox/profiles-a" app.mobileprovision "$APP_NAME" 11111111-1111-1111-1111-111111111111 "$TEAM" "$APP_ID" "$FUTURE"
before_project=$(checksum "$BTLAP_PROJECT"); before_test=$(checksum "$BTLAP_PINNING_TEST")
run enable
expect_status 1 "$status" "$out"
expect_contains "$out" "blocked: profile \"$APP_NAME\" (11111111-1111-1111-1111-111111111111) lacks aps-environment"
expect_contains "$out" "already enabled, but check is blocked: see the blocked: lines above; a device build will not sign until they are fixed"
expect_unchanged "$BTLAP_PROJECT" "$before_project" "project file (enabled but blocked)"
expect_unchanged "$BTLAP_PINNING_TEST" "$before_test" "pinning test (enabled but blocked)"
finish

begin "enable: an unrecognised project state is refused"
setup; ready_profiles
perl -0777 -pi -e 's/CODE_SIGN_ENTITLEMENTS = Apps\/BabyTracker\.entitlements;/CODE_SIGN_ENTITLEMENTS = Apps\/Other.entitlements;/' "$BTLAP_PROJECT"
before_test=$(checksum "$BTLAP_PINNING_TEST")
run enable
expect_status 1 "$status" "$out"
expect_contains "$out" "unexpected project state"
expect_unchanged "$BTLAP_PINNING_TEST" "$before_test" "pinning test (unexpected state)"
finish

begin "enable: a pinning test that does not match the project state is refused"
setup; ready_profiles
perl -0777 -pi -e 's/pushEntitlementsAreSelectedOnlyForTheSimulatorSDK/somethingElse/' "$BTLAP_PINNING_TEST"
before_project=$(checksum "$BTLAP_PROJECT")
run enable
expect_status 1 "$status" "$out"
expect_contains "$out" "pinning test"
expect_unchanged "$BTLAP_PROJECT" "$before_project" "project file (pinning mismatch)"
finish

# ---------------------------------------------------------------- state

begin "state: the fixture is pre-switch"
setup
run state
expect_status 0 "$status" "$out"
expect_contains "$out" "pre-switch"
finish

begin "state: the real project file is in a state the script recognises"
unset BTLAP_PROFILE_DIRS BTLAP_PROJECT BTLAP_PINNING_TEST BTLAP_NOW BTLAP_CODESIGN
out=$(cd "$repo" && "$script" state 2>&1); status=$?
expect_status 0 "$status" "$out"
finish

begin "usage: unknown mode"
out=$("$script" frobnicate 2>&1); status=$?
expect_status 2 "$status" "$out"
expect_contains "$out" "usage"
finish

echo
echo "live-activity-push-signing: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
