# Baby Tracker sync node over Tailscale

**Date:** 2026-09-24  
**Scope:** Research and read-only inspection only. No Unraid, Tailscale, container,
or application configuration was changed.

## Recommendation

Run the Baby Tracker server and a **dedicated Tailscale sidecar in one Compose
project**, with the application sharing the sidecar's network namespace. Give
that Tailscale node a stable machine name such as `baby-sync`, persist its
Tailscale state, and let Tailscale Serve terminate HTTPS on:

```text
https://baby-sync.example-tailnet.ts.net
```

Do not publish the Baby Tracker application or PostgreSQL on an Unraid host
port. Allow only the two caregiver identities to reach the sidecar on TCP 443
through Tailscale grants.

This is the best fit for this homelab because it gives the sync service its own
DNS/TLS and access-control identity without changing the existing Unraid
Tailscale node, native nginx, or any unrelated container. Tailscale explicitly
describes the sidecar pattern as merging the application and Tailscale network
namespaces, with no other ingress required, and its current Docker integration
supports persistent state and declarative Serve configuration through
`TS_STATE_DIR`, `TS_AUTH_ONCE`, `TS_HOSTNAME`, and `TS_SERVE_CONFIG`.
([Docker sidecar pattern](https://tailscale.com/blog/docker-tailscale-guide),
[Docker parameters](https://tailscale.com/docs/features/containers/docker/docker-params))

## Facts established on this Unraid host

Read-only inspection of `root@192.0.2.10` found:

- The Unraid host is already an online Tailscale node running 1.98.3 at
  `<host>.example-tailnet.ts.net`.
- MagicDNS suffix is `example-tailnet.ts.net`.
- HTTPS is already enabled for the tailnet. The host currently presents a valid
  Let's Encrypt certificate for its exact `.ts.net` FQDN.
- Native Unraid nginx already owns ports 80 and 443 on the host's LAN,
  loopback, and Tailscale addresses.
- The host has no active Tailscale Serve configuration and has a user identity,
  not a tag identity.
- The checked-in Baby Tracker Compose file publishes the API only on host
  loopback (`127.0.0.1:${BT_HTTP_PORT}:8080`) and does not publish PostgreSQL.
  The server requires a canonical HTTPS `BT_PUBLIC_ORIGIN` outside development.
  See [`Server/compose.yaml`](../../Server/compose.yaml) and
  [`Server/README.md`](../../Server/README.md).

These facts make host port 443 unavailable to ordinary host-level Serve and
make changing the host's identity unnecessarily risky.

## Why the `.ts.net` origin works for the iOS client

MagicDNS assigns each tailnet node a fully qualified name of the form
`machine-name.tailnet-name.ts.net`. Devices signed in to the tailnet can resolve
and reach it, including shared machines when the full FQDN is used.
([MagicDNS](https://tailscale.com/docs/features/magicdns))

When HTTPS is enabled, Tailscale can provision a publicly trusted Let's Encrypt
certificate for the **exact node FQDN**. This is not a wildcard certificate that
we manage. The node and tailnet names appear in public Certificate Transparency
records, but the service remains reachable only through Tailscale. Certificate
and ACME private keys are generated and retained on the node.
([Tailscale HTTPS certificates](https://tailscale.com/docs/how-to/set-up-https-certificates))

Tailscale Serve terminates HTTPS and reverse-proxies to a local HTTP listener.
Its HTTPS certificate is automatic. A background Serve configuration survives
device reboot and `tailscale down`/`up`; container deployments can provide a
declarative `TS_SERVE_CONFIG` file instead.
([Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve),
[`tailscale serve` reference](https://tailscale.com/docs/reference/tailscale-cli/serve))

This matches the application contract: `ServerOrigin` accepts HTTPS origins
with a hostname and no path, query, or fragment, and Apple App Transport
Security requires `URLSession` traffic to use HTTPS and pass normal TLS trust
evaluation. A publicly trusted Let's Encrypt certificate needs no ATS exception
or custom trust code.
([Apple ATS](https://developer.apple.com/documentation/security/preventing-insecure-network-connections),
[`ServerOrigin.swift`](../../Packages/BabyTracker/Sources/BabySync/ServerOrigin.swift))

## Options compared

| Option | Advantages | Problems in this homelab | Verdict |
| --- | --- | --- | --- |
| Existing host Tailscale + ordinary Serve | Fewest new moving parts; proxies directly to a loopback-bound API; `-bg` persists | Unraid nginx already owns the host's Tailscale port 443. A nonstandard HTTPS port would leak host/service identity together and require the port in the canonical origin. Access policy applies to the broad host identity rather than only Baby Tracker. | Avoid. |
| Existing host + Tailscale Service (`serve --service`) | A Service gets a distinct stable MagicDNS name and TailVIP, can use HTTPS 443 without colliding with host 443, and can later move or gain multiple backends | Every Service host must use a tag-based identity. Applying a tag to the current user-authenticated Unraid node removes its user identity, potentially changing access to every service already on that host. It also adds Service definition/advertisement approval for a single backend. | Technically strong, but do not retrofit it onto this shared Unraid host. Reconsider for future HA using dedicated tagged hosts. |
| Dedicated Tailscale sidecar + ordinary Serve | Dedicated FQDN, TLS key, state, tag, ACL/grant target, and lifecycle; no host port; no changes to Unraid nginx or other containers; can be shared with one external user | Adds one small container and a protected state volume; the app container must share its network namespace; requires bootstrapping a tagged node | **Recommended.** |
| Direct host port over tailnet | Simple transport path | Raw HTTP fails the app's HTTPS-only origin and ATS. TLS on a direct port still needs certificate renewal and a TLS server/proxy; host 443 is occupied. Binding the API to a host/tailnet address weakens isolation. | Reject. |

### Tailscale Services note

Tailscale Services are not merely another host port: each Service has a stable
MagicDNS name and TailVIP, and `tailscale serve --service=svc:baby-sync
--https=443 127.0.0.1:8080` can provision TLS for that Service. However, the
host must be tag-authenticated, not user-authenticated. Current clients 1.94+
discover Services without `accept-routes`.
([Tailscale Services](https://tailscale.com/docs/features/tailscale-services))

The installed Unraid Tailscale 1.98.3 supports this. The blocker is not client
capability; it is the blast radius of changing the identity of a shared host.
A future deployment with two dedicated tagged service hosts could use a
Tailscale Service to keep the public-in-tailnet origin stable across failover.

## Sidecar deployment shape

The eventual Compose change should be limited to the Baby Tracker project:

1. Add the official Tailscale container with a persistent, private state volume,
   a stable `TS_HOSTNAME=baby-sync`, `TS_STATE_DIR`, `TS_AUTH_ONCE=true`, and a
   mounted `TS_SERVE_CONFIG` that maps HTTPS 443 to
   `http://127.0.0.1:8080`.
2. Put only the Baby Tracker app container in
   `network_mode: service:<tailscale-sidecar>`. The app can continue listening
   on `:8080` inside that shared namespace. Remove its host `ports` mapping.
3. Keep PostgreSQL on the private Compose network with no published port. The
   sidecar is an ingress for the API, not a subnet router.
4. Persist `/var/lib/tailscale`; treat it as sensitive node identity state. Use
   a one-off, tagged, optionally pre-approved auth key for initial registration,
   then remove the bootstrap credential. Tagged server nodes have key expiry
   disabled by default; the key used to register them may expire without
   deauthorizing the already registered node.
   ([Auth keys](https://tailscale.com/docs/features/access-control/auth-keys),
   [tags](https://tailscale.com/docs/features/tags))
5. Set `BT_PUBLIC_ORIGIN=https://baby-sync.example-tailnet.ts.net`. Keep
   `BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32`, because Serve and the app share the
   namespace and the proxy target is loopback. Verify the server advertises the
   exact same canonical origin at `/v1/server`.
6. Define a narrowly owned `tag:baby-sync` and a grant that permits only the two
   caregiver user identities to reach that tag on TCP 443. Grants are
   deny-by-default and can select user/group sources and tagged destinations.
   ([Tailscale grants](https://tailscale.com/docs/features/access-control/grants))

Pin the Tailscale container image to an explicit version or image digest in the
deployment rather than `latest`, and enable its health endpoint so readiness can
cover the sidecar as well as the API.

## Caregiver and iPhone requirements

Both caregiver iPhones must have an active Tailscale path whenever the app needs
to sync. The caregivers do **not** strictly have to belong to the same tailnet:

- Simplest: invite the second caregiver as a Member of the existing tailnet, then restrict
  them with a grant to only `tag:baby-sync:443`.
- More isolated: share only the `baby-sync` sidecar machine with her Tailscale
  user. A shared machine appears in her own tailnet, remains private, and is
  reachable by its full MagicDNS FQDN; both tailnets' policies must allow it.

Inviting a user grants tailnet membership (constrained by access controls),
whereas machine sharing exposes only that machine and does not add her to the
whole tailnet.
([invite versus share](https://tailscale.com/docs/reference/inviting-vs-sharing),
[machine sharing](https://tailscale.com/docs/features/sharing))

On iOS, Tailscale installs a Network Extension VPN configuration. VPN On Demand
can keep it connected on Wi-Fi and cellular, or trigger it when iOS sees a
hostname ending in `*.ts.net`. The hostname trigger is available when the
interface rule is set to **Do Nothing**; a conflicting **Never** rule will
prevent connection.
([Tailscale VPN On Demand for iOS](https://tailscale.com/docs/features/client/ios-vpn-on-demand))

Consequences:

- If either caregiver disables Tailscale, API sync and the private invitation
  handoff page will be unavailable until it reconnects.
- This self-hosted topology does not become public merely because the TLS
  certificate is publicly trusted.
- Apple Sign in and Baby Tracker family roles remain the application-level
  identity/authorization layer. Tailscale is an additional network gate, not a
  replacement.

## Invitation links and Universal Links

The current server creates
`https://baby-sync.example-tailnet.ts.net/invite#token=...`. The private web page
then constructs a `babytracker://invite?...` URL to hand the token to the app.
This flow does not depend on Universal Links or an
`apple-app-site-association` file; it only requires Safari to reach the private
`.ts.net` page through Tailscale. See
[`invitation.html`](../../Server/internal/httpapi/assets/invitation.html).

Do not plan production Universal Links on this private `.ts.net` origin.
Starting with iOS 14, Associated Domains normally fetches the AASA file through
Apple's public CDN. Apple documents a direct-fetch alternate mode for private
servers during development, but a tailnet-only server is not reachable by that
CDN and developer mode is not an end-user deployment mechanism. A future true
Universal Link should use a separate public HTTPS handoff domain with a valid
AASA file, while keeping the sync API private.
([Apple: Supporting associated domains](https://developer.apple.com/documentation/xcode/supporting-associated-domains),
[WWDC20: What's new in Universal Links](https://developer.apple.com/videos/play/wwdc2020/10098/))

## Verification required after deployment

No deployment claim should be made until all of these pass:

1. Sidecar is the only new Tailscale node/container and unrelated container
   names, images, uptime, and published ports are unchanged.
2. From each caregiver iPhone on cellular, with Wi-Fi off:
   `https://baby-sync.example-tailnet.ts.net/healthz` returns healthy and
   `/readyz` returns ready without a certificate warning.
3. `/v1/server` reports the exact `.ts.net` canonical origin, expected instance
   ID, source revision, and capabilities.
4. The TLS certificate hostname and chain validate through ordinary
   `URLSession`; there is no ATS exception or trust bypass.
5. A third, unauthorized tailnet user/device cannot connect to TCP 443.
6. The second caregiver can open the invitation link, hand it into Baby Tracker, join as a
   caregiver, and both phones can create and observe records in both directions.
7. Restart only the Baby Tracker Compose project and prove the sidecar retains
   its node identity/FQDN, Serve resumes, migrations remain applied, and the API
   returns healthy and ready.
8. Confirm WebSocket live hints and ordinary HTTPS reconciliation both work
   through Serve; a healthy probe alone does not prove synchronization.

## Bottom line

Use a dedicated, tagged `baby-sync` Tailscale sidecar with ordinary Tailscale
Serve now. It is private, iOS-compatible HTTPS, smallest in blast radius, and
can be shared with exactly one additional caregiver without opening the rest of
Unraid. Keep Tailscale Services as a later HA/migration upgrade, not as a reason
to retag the existing multipurpose Unraid host.
