# Private Family Sync Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy and prove private two-caregiver synchronization through a dedicated Tailscale sidecar on Unraid without changing unrelated containers.

**Architecture:** One isolated `baby-tracker-sync` Compose project owns PostgreSQL, API, migration tooling, and a tagged `baby-sync` Tailscale sidecar. The app shares the sidecar network namespace, Tailscale Serve terminates trusted HTTPS, and the iOS client confirms the exact `.ts.net` origin and server instance before authentication.

**Tech Stack:** Go server, PostgreSQL 17, Docker Compose on Unraid, official Tailscale container/Serve, SwiftUI server selection, Sign in with Apple.

**Spec:** `docs/superpowers/specs/2026-09-24-private-family-sync-deployment-design.md`

## Global Constraints

- Touch only the `baby-tracker-sync` project and its appdata directory.
- Never use broad Docker cleanup, `compose down --volumes`, or another stack's network/volume.
- Do not print, commit, or retain secret values in logs or chat.
- Pin all third-party container images to immutable digests.
- Production claims require deployed SHA, migrations, public-in-tailnet readiness, database evidence, and bounded logs.
- Both caregivers must pass a real two-device bidirectional sync test.

## Review Focus

- Sidecar restart keeps the exact node identity and certificate hostname.
- A ready HTTP probe does not mask failed Apple authentication or record sync.
- A wrong server instance at the same origin stops before credentials upload.
- An unauthorized tailnet identity cannot reach API port 443.
- Restart/migration operations target only the named Baby Tracker project.

---

### Task 1: Add hardened Unraid deployment assets

**Files:**
- Create: `Server/deploy/unraid/compose.yaml`
- Create: `Server/deploy/unraid/tailscale/serve.json`
- Create: `Server/scripts/provision-unraid-secrets.sh`
- Create: `Server/scripts/verify-unraid-deployment.sh`
- Modify: `Server/README.md`
- Test: `Server/scripts/unraid_deployment_test.go`

**Interfaces:**
- Produces: deterministic stack config rooted at `BT_DEPLOY_ROOT`, a secret provisioner that is idempotent without overwriting existing secrets, and a non-secret verification report.

- [ ] **Step 1: Write failing deployment-structure tests**

Parse merged Compose config and assert project services, pinned images, no database/HTTP host publication, read-only API rootfs, dropped capabilities, private state mounts, Tailscale healthcheck, and `network_mode: service:tailscale`.

- [ ] **Step 2: Run RED**

Run: `rtk proxy sh -c 'cd Server && go test ./scripts -run TestUnraidDeployment -count=1'`

- [ ] **Step 3: Implement assets and safe provisioner**

The provisioner accepts an explicit absolute deployment root, uses `umask 077`, creates only named files, refuses symlinks/pre-existing malformed files, and emits no secret bytes. The verifier compares baseline/current unrelated container metadata and probes only the Baby Tracker project.

- [ ] **Step 4: Validate Compose and tests GREEN**

Run the Go test and `docker compose -f Server/compose.yaml -f Server/deploy/unraid/compose.yaml config --quiet` with non-secret fixture variables.

- [ ] **Step 5: Commit deployment assets**

```bash
rtk git add Server/deploy/unraid Server/scripts/provision-unraid-secrets.sh Server/scripts/verify-unraid-deployment.sh Server/scripts/unraid_deployment_test.go Server/README.md
rtk git commit -m "build(server): add isolated Unraid deployment"
```

### Task 2: Make server confirmation reachable on iOS

**Files:**
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/FamilySyncComposition.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/FamilySettingsView.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/ServerSelectionView.swift`
- Test: `Packages/BabyTracker/Tests/SettingsFeatureTests/ServerSelectionTests.swift`
- Test: `UITests/BabyTrackerUITests/CaregiverOnboardingTests.swift`

**Interfaces:**
- Produces: a reachable confirmation flow for `https://baby-sync.example-tailnet.ts.net` bound to the deployed instance UUID.

- [ ] **Step 1: Add failing onboarding tests**

Assert local-only Caregivers first shows server choice, probing displays exact origin/instance/source version, confirmation precedes Sign in with Apple, and an instance mismatch never sends identity proof.

- [ ] **Step 2: Run RED**

Run: `rtk make test TEST_FILTER='ServerSelection|FamilySettings'`.

- [ ] **Step 3: Wire hosted endpoint and confirmation navigation**

Keep `ConfirmedEndpoint` immutable and compile the deployment's stable instance UUID only after Task 4 creates it. Do not auto-confirm or bypass the probe.

- [ ] **Step 4: Run tests GREEN and commit**

```bash
rtk make test TEST_FILTER='ServerSelection|FamilySettings'
rtk git add Packages/BabyTracker/Sources/SettingsFeature Packages/BabyTracker/Tests/SettingsFeatureTests UITests/BabyTrackerUITests Makefile
rtk git commit -m "feat(sync): expose private server onboarding"
```

### Task 3: Pre-deployment source and host gates

**Files:**
- Create: `docs/verification/2026-09-24-private-family-sync-deployment.md`

**Interfaces:**
- Produces: exact source SHA, image inputs, host baseline, and pass/fail gate record.

- [ ] **Step 1: Run source gates**

Run `rtk make server-test`, `rtk make server-build`, `rtk make server-smoke`, `rtk make test`, and `rtk make build`. Stop on any source failure.

- [ ] **Step 2: Capture Unraid baseline read-only**

Record container name, ID, image ID, status, ports, networks, and start time for every existing container; record Tailscale version/DNS name/Serve state without environment variables or secrets.

- [ ] **Step 3: Confirm exact deployment targets**

Require `/mnt/user/appdata/baby-tracker-sync` absent or owned solely by this deployment, ports unpublished, and Compose project name unused.

### Task 4: Provision and initialize the isolated server

**Files:**
- External target: `/mnt/user/appdata/baby-tracker-sync/`
- Modify after allocation: `FamilySyncComposition.hostedEndpoint` instance UUID.

**Interfaces:**
- Produces: stable server instance UUID, schema-ready API on the sidecar loopback namespace, and exact source-labeled image.

- [ ] **Step 1: Transfer reviewed deployment source only**

Create the exact appdata deployment directory, transfer a Git archive of the reviewed SHA, and verify its hash on Unraid. Do not copy `.git`, local build products, or credential files.

- [ ] **Step 2: Provision secrets without disclosure**

Run the checked-in provisioner on Unraid. Verify file names, owners, modes, and hashes-only inventory; never read values back into the transcript.

- [ ] **Step 3: Build and start database/API only**

Use `docker compose --project-name baby-tracker-sync ... up -d --build --wait db app`; require only the two new containers and readiness 503 before migration.

- [ ] **Step 4: Apply migration, authority initialization, and reconciliation**

Run the exact README sequence through the project-scoped migrate service. Require schema contiguity, authority verification, health 200, readiness 200, and exact `/v1/server` origin/instance/source SHA.

- [ ] **Step 5: Bind the stable instance into the iOS endpoint and rerun tests**

Update only the constant/test fixture, run focused Settings tests, and commit:

```bash
rtk git add Packages/BabyTracker/Sources/SettingsFeature/FamilySyncComposition.swift Packages/BabyTracker/Tests/SettingsFeatureTests
rtk git commit -m "chore(sync): bind the private server identity"
```

### Task 5: Enroll the dedicated Tailscale sidecar

**Files:**
- External state: `/mnt/user/appdata/baby-tracker-sync/tailscale-state`
- External policy: Tailscale `tag:baby-sync` ownership and caregiver-only TCP 443 grant.

**Interfaces:**
- Produces: `https://baby-sync.example-tailnet.ts.net` with trusted TLS and no host port.

- [ ] **Step 1: Create the tag and narrow grant**

In the Tailscale admin policy, make an administrator-owned `tag:baby-sync` and grant only the two named caregiver identities TCP 443 to that tag. Validate policy before saving.

- [ ] **Step 2: Create one pre-authorized, tagged, one-use auth key**

Supply it to the sidecar through a root-readable secret file, start only the Baby Tracker sidecar, then delete the bootstrap file after the node reaches online state.

- [ ] **Step 3: Verify Serve and TLS**

Require the exact FQDN, valid public chain, `/healthz` 200, `/readyz` 200, `/v1/server` canonical identity, and WebSocket upgrade through Serve.

- [ ] **Step 4: Restart only the sidecar and prove identity persistence**

Record node ID/FQDN before and after project-scoped restart; require equality and resumed Serve configuration.

### Task 6: Build, sign, install, and complete two-caregiver acceptance

**Files:**
- Modify: `docs/verification/2026-09-24-private-family-sync-deployment.md`

**Interfaces:**
- Consumes: live server and iOS endpoint.
- Produces: direct two-device sync evidence.

- [ ] **Step 1: Build and install the signed app on both caregiver iPhones**

Use automatic signing under team `TEAM123456`; register the second device only with explicit device access. Verify Sign in with Apple entitlement and ordinary TLS trust.

- [ ] **Step 2: Confirm endpoint and create family on owner phone**

Probe and confirm the exact origin/instance, sign in with Apple, create the family, and create a caregiver invitation.

- [ ] **Step 3: Join on second caregiver’s phone**

With Tailscale active on cellular, open the private invitation handoff, confirm the same endpoint, sign in independently, and join as caregiver.

- [ ] **Step 4: Prove bidirectional records and timers**

Create distinct records on each phone, observe them on the other, start/switch/stop a timer across caregivers, and prove both live hints and foreground reconciliation.

- [ ] **Step 5: Prove denial and restart durability**

Use an unauthorized tailnet identity/device to show TCP 443 denial. Restart only `baby-tracker-sync`; verify records, server identity, sidecar identity, and readiness remain intact.

- [ ] **Step 6: Reconcile the unrelated-container baseline**

Compare IDs/images/ports/statuses. Any unrelated difference blocks the safety claim until explained.

- [ ] **Step 7: Commit sanitized evidence**

```bash
rtk git add docs/verification/2026-09-24-private-family-sync-deployment.md
rtk git commit -m "docs: verify private caregiver synchronization"
```

