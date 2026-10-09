# Private Family Sync Deployment — Design Spec

**Date:** 2026-09-24  
**Status:** Approved for implementation  
**Research:** `docs/research/2026-09-24-tailscale-sync-node.md`

## Purpose

Deploy the existing self-hosted Baby Tracker synchronization server on Unraid
for two caregivers without exposing infant-health data to the public internet
or modifying unrelated containers.

## Topology

- A dedicated Compose project named `baby-tracker-sync` owns exactly the API,
  PostgreSQL, migration tool, and Tailscale sidecar.
- The Tailscale node is named `baby-sync`, has a dedicated tagged identity, and
  serves `https://baby-sync.example-tailnet.ts.net` on HTTPS 443.
- The API shares the sidecar network namespace and listens on `127.0.0.1:8080`.
  Tailscale Serve is its only ingress. No Baby Tracker port is published on the
  Unraid host.
- PostgreSQL stays on a private Compose network with no published port.
- Existing Unraid Tailscale, nginx, Cloudflare, and every unrelated container
  remain unchanged.

## Identity and access

- The second caregiver is already a member of the owner's tailnet.
- The sidecar uses `tag:baby-sync`; tag ownership and a narrow grant allow only
  the two caregiver identities to reach TCP 443.
- Tailscale is an additional network boundary. Sign in with Apple, family
  membership, owner/caregiver roles, invitations, and server sessions remain
  mandatory application controls.
- Bootstrap authentication uses a one-time, tagged, pre-authorized key supplied
  as a secret. Persistent Tailscale state retains node identity; the bootstrap
  key is removed after enrollment.

## Durable state and secrets

- Persistent directories live under
  `/mnt/user/appdata/baby-tracker-sync/` with separate `postgres`,
  `security-ledger`, `ledger-head`, `tailscale-state`, `secrets`, and deployment
  directories.
- Generate independent database passwords and every required 32-byte pepper on
  Unraid with restrictive permissions. Never print secret values or place them
  in Compose environment variables, Git, or chat output.
- The stable server instance UUID, recovery epoch, public origin, source SHA,
  and non-secret deployment manifest are recorded. The actor-ID pepper,
  keyrings, database, ledger, and head are one coordinated backup set.
- Images are pinned to immutable digests. The Baby Tracker image is built from
  the exact reviewed source SHA and labeled with it.

## Deployment sequence

1. Capture a read-only baseline of all container IDs/images/statuses/ports.
2. Run source tests, protocol E2E, image build, and local smoke gates.
3. Create only the Baby Tracker appdata directories and secrets.
4. Start the private database and API; expect health 200 and readiness 503.
5. Apply migrations, initialize authority, and reconcile; require readiness 200.
6. Enroll the dedicated sidecar, apply declarative Serve configuration, and
   verify the exact `.ts.net` certificate and canonical `/v1/server` identity.
7. Remove the bootstrap auth key and prove the sidecar survives restart with the
   same node identity.
8. Re-audit unrelated containers against the baseline.

No command may use `docker compose down` without the exact Baby Tracker project
name. `--volumes`, broad Docker pruning, and mutation of another stack are
forbidden.

## iOS integration

- Wire the deployed origin and stable instance ID into the server-selection
  flow; do not silently upload to an unconfirmed server.
- Make server selection reachable from Caregivers. The user sees and confirms
  the canonical origin and instance identity before Sign in with Apple.
- Keep the private invitation handoff page and `babytracker://` callback.
  Universal Links are not required for the private `.ts.net` origin.
- Both phones use Tailscale VPN On Demand for the `.ts.net` suffix so foreground
  sync works on Wi-Fi and cellular.

## Verification and completion boundary

Deployment is complete only with fresh evidence for all of the following:

- exact deployed source SHA and immutable image digest;
- migration version/contiguity and authority reconciliation;
- local health plus Tailscale HTTPS readiness and `/v1/server` identity;
- ordinary TLS trust from both iPhones without ATS exceptions;
- bounded API/database logs with no readiness, auth, or projection errors;
- unauthorized tailnet access denied;
- owner creates an invitation, second caregiver joins as caregiver, and each phone creates a
  record that appears on the other through both WebSocket hints and ordinary
  reconciliation;
- restart of only the Baby Tracker project preserves data, server identity,
  sidecar identity, and readiness;
- unrelated container IDs/images/ports/statuses remain unchanged.

If the second phone or a required Tailscale/App Store authentication prompt is
not available, report that acceptance item as blocked rather than claiming the
deployment complete.

