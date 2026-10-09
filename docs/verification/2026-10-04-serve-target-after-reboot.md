# Sync 502 after an Unraid reboot (2026-10-04)

## Symptom

Phones stopped syncing after Unraid rebooted at 03:08 (server time). All three `baby-tracker-sync` containers reported healthy, but `https://baby-sync.example-tailnet.ts.net/healthz` returned 502. The Tailscale container logged `proxy error: dial tcp: lookup app on 127.0.0.11:53: no such host`. The same error had appeared intermittently since 2026-09-29.

## Cause

At boot, `/etc/rc.d/rc.docker` restores each container's stored MAC address. For `baby-tracker-sync-app-1` it disconnected the container from `baby-tracker-sync_default` and reconnected it through the Docker API with only the MAC and IP in `EndpointConfig` (syslog: `reconnecting baby-tracker-sync-app-1 to network baby-tracker-sync_default to restore MAC …`). That reconnect drops the compose aliases. The container came back with `Aliases=[]`, so `app` stopped resolving. Its container name stayed in `DNSNames`.

## Fix

- Hotfix, applied in place: reconnect the app container with `--ip 172.31.0.2 --alias app --alias baby-tracker-sync-app-1`.
- Durable: `Server/deploy/unraid/tailscale/serve.json` now proxies to `http://baby-tracker-sync-app-1:8080`. Docker always registers the container name, whatever happens to the aliases. `TestUnraidServeConfigTerminatesHTTPSOnlyForBabySync` requires the container-name target and rejects `http://app:`.
