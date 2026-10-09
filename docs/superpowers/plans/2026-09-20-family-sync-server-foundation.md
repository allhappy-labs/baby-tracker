# Family Sync Server Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the first independently runnable family-sync slice: a secure Go service skeleton, checked-in protocol foundation, explicit PostgreSQL migrations and tenant boundary, and a provider-neutral container stack with proven health and readiness behavior.

**Architecture:** `Server/` is a separate Go module and modular monolith. The HTTP process depends on narrow readiness and database interfaces; the migration CLI is a distinct entry point and the server never migrates on startup. PostgreSQL is authoritative, tenant tables are protected by forced row-level security, and Docker Compose is development/self-host scaffolding rather than a production claim.

**Tech Stack:** Go 1.26.2, standard-library `net/http` and `log/slog`, `github.com/jackc/pgx/v5` v5.11.0, `gopkg.in/yaml.v3` v3.0.1 for contract validation, PostgreSQL 17, OpenAPI 3.1, Docker/Compose.

**Spec:** `docs/superpowers/specs/2026-09-19-family-sync-server-design.md`

## Global Constraints

- Work only in `<repo>/.worktrees/shared-family-server`; preserve the separate primary checkout and its state.
- Follow test-driven development: add one behavioral test, run it and capture the expected failure, implement the minimum, then rerun it. Pure deployment files use a failing validation or smoke command before creation.
- Use Go 1.26.2. Pin `github.com/jackc/pgx/v5` to `v5.11.0` and `gopkg.in/yaml.v3` to `v3.0.1` when their owning tasks introduce them; `go.sum` remains committed.
- Use only standard-library HTTP routing and structured `log/slog` in this plan. Do not add a web framework, ORM, migration library, Redis, or Kubernetes.
- All non-development public origins are canonical HTTPS origins with no userinfo, query, fragment, or path other than `/`. Never add a TLS trust bypass.
- Logs and metrics may contain request IDs and opaque IDs only. Never emit tokens, secrets, names, notes, record payloads, database URLs, or request bodies.
- The application process never runs migrations. `baby-tracker-admin migrate` is the only migration entry point introduced here.
- Runtime and migration database roles are separate. The runtime role is not a superuser, does not own tenant tables, and cannot bypass row-level security. Tenant policies are `FORCE ROW LEVEL SECURITY` and default deny when transaction-local family/account settings are absent.
- Health is dependency-independent. This foundation's readiness fails for an unavailable database or unapplied migrations. The next identity/lifecycle plan adds recovery-quarantine and security-ledger-head gates before any authenticated traffic exists.
- Container stages and Compose service images are pinned by immutable digest. Required multi-architecture targets are `linux/amd64` and `linux/arm64`.
- Third-party CI actions, when added in a later plan, must be pinned to immutable commit SHAs.
- This plan creates attachment metadata schemas/interfaces only. It must not create upload/download endpoints, object storage, blob tables, or attachment UI.
- Use Conventional Commits and stage only this task's files. Do not push, merge, publish, or deploy.
- Run every shell command through `rtk`.

## Review Focus

- Missing or malformed production configuration must fail closed without logging secret values; Task 1 tests empty values, unsafe origins, bad UUIDs, and invalid recovery epochs.
- Liveness must stay healthy while readiness truthfully fails; Task 2 tests independent probe behavior and privacy-safe request logs.
- A server started against pending migrations must remain unready and must not migrate implicitly; Task 3 tests both states against PostgreSQL.
- A pooled connection without tenant context must see no family rows, and a connection reused for another family must not retain prior context; Task 4 tests both cases through the runtime role.
- Contract and deployment artifacts must describe/run the same routes and versions; Tasks 5 and 6 test OpenAPI responses and the built container rather than relying on text inspection alone.

## File Structure

```text
Server/
  go.mod, go.sum                       module boundary and locked dependency graph
  cmd/baby-tracker-server/main.go      process wiring and graceful shutdown
  cmd/baby-tracker-admin/main.go       explicit migration command
  internal/config/config.go            environment parsing and fail-closed validation
  internal/httpapi/router.go           route registration and JSON response boundary
  internal/httpapi/middleware.go       request IDs, recovery, structured access logs
  internal/httpapi/operational.go      health, readiness, and server-info handlers
  internal/probe/probe.go              bounded in-image HTTP health probe
  internal/database/pool.go            pgx pool creation and health checks
  internal/migrate/migrate.go          embedded ordered SQL migration runner
  internal/migrate/status.go           required/current migration comparison
  internal/tenant/context.go           transaction-local RLS context
  internal/protocol/types.go           stable error/cursor/attachment metadata types
  migrations/embed.go                  owns the embedded migration filesystem
  migrations/000001_server_state.sql   server identity/recovery metadata
  migrations/000002_tenancy.sql        tenant foundation and forced RLS
  api/openapi.yaml                     OpenAPI 3.1 source of truth
  api/fixtures/*.json                  shared language-neutral contract examples
  Dockerfile, .dockerignore            pinned multi-stage non-root image
  compose.yaml                         local/self-host development stack
  README.md                            exact run, migrate, probe, backup-boundary notes
Makefile                               server verification entry points
```

---

### Task 1: Go module and fail-closed configuration

**Files:**
- Create: `Server/go.mod`
- Create: `Server/internal/config/config.go`
- Test: `Server/internal/config/config_test.go`

**Interfaces:**
- Consumes: environment values through injected `func(string) string`
- Produces: `config.Load(getenv func(string) string) (config.Config, error)`; `config.Mode` values `development`, `hosted`, `self_hosted`; `Config.PublicOrigin *url.URL`; `Config.RecoveryEpoch uint64`

- [ ] **Step 1: Initialize the module and write failing table tests**

Create `Server/go.mod` with module `babytracker/server` and `go 1.26.2`. In `config_test.go`, use a map-backed getter and pin these cases:

```go
func TestLoadRejectsUnsafeProductionConfiguration(t *testing.T) {
    base := validHostedEnvironment()
    tests := map[string]func(map[string]string){
        "missing database": func(v map[string]string) { delete(v, "BT_DATABASE_URL") },
        "http public origin": func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "http://sync.example.test" },
        "origin path": func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "https://sync.example.test/api" },
        "origin credentials": func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "https://user:pass@sync.example.test" },
        "invalid instance id": func(v map[string]string) { v["BT_SERVER_INSTANCE_ID"] = "shared" },
        "zero recovery epoch": func(v map[string]string) { v["BT_RECOVERY_EPOCH"] = "0" },
        "missing ledger directory": func(v map[string]string) { delete(v, "BT_SECURITY_LEDGER_DIR") },
        "same ledger and head directory": func(v map[string]string) { v["BT_LEDGER_HEAD_DIR"] = v["BT_SECURITY_LEDGER_DIR"] },
    }
    for name, mutate := range tests {
        t.Run(name, func(t *testing.T) {
            values := maps.Clone(base)
            mutate(values)
            if _, err := Load(mapGetter(values)); err == nil { t.Fatal("Load succeeded") }
        })
    }
}

func TestLoadCanonicalizesHostedConfiguration(t *testing.T) {
    values := validHostedEnvironment()
    values["BT_PUBLIC_ORIGIN"] = "https://SYNC.EXAMPLE.TEST:443/"
    cfg, err := Load(mapGetter(values))
    if err != nil { t.Fatal(err) }
    if got := cfg.PublicOrigin.String(); got != "https://sync.example.test" { t.Fatalf("origin = %q", got) }
    if cfg.ListenAddress != ":8080" || cfg.RecoveryEpoch != 7 { t.Fatalf("cfg = %#v", cfg) }
}
```

The valid hosted fixture contains `BT_DATABASE_URL`, `BT_PUBLIC_ORIGIN`, a lowercase RFC 4122 UUID in `BT_SERVER_INSTANCE_ID`, `BT_RECOVERY_EPOCH=7`, distinct absolute `BT_SECURITY_LEDGER_DIR` and `BT_LEDGER_HEAD_DIR`, `BT_APPLE_AUDIENCE`, `BT_SESSION_SIGNING_KEY_FILE`, and `BT_TRUSTED_PROXY_CIDRS`.

- [ ] **Step 2: Verify RED**

Run: `rtk go test ./internal/config -run TestLoad -v` from `Server/`\
Expected: FAIL because `Load`, `Config`, and `Mode` do not exist.

- [ ] **Step 3: Implement the minimum configuration loader**

`Config` contains `Mode`, `ListenAddress`, `DatabaseURL`, `PublicOrigin`, `ServerInstanceID`, `RecoveryEpoch`, `SecurityLedgerDir`, `LedgerHeadDir`, `AppleAudience`, `SessionSigningKeyFile`, and parsed `TrustedProxyCIDRs []netip.Prefix`. `Load` aggregates field errors with `errors.Join`, never includes secret values in errors, lowercases the host, removes default port 443 and root path, rejects non-HTTPS outside development, rejects IP/userinfo/query/fragment origins, validates a lowercase hyphenated UUID, requires positive recovery epoch, absolute distinct ledger/head directories, and defaults the listen address to `:8080`.

- [ ] **Step 4: Verify GREEN and module hygiene**

Run: `rtk go test ./internal/config -v && rtk go mod tidy && rtk go test ./...` from `Server/`\
Expected: all tests PASS; `go.mod` remains dependency-free until the database task and `go.sum` is absent or empty.

- [ ] **Step 5: Commit**

```bash
rtk git add Server/go.mod Server/internal/config
rtk git commit -m "feat(server): add validated runtime configuration"
```

---

### Task 2: Operational HTTP surface and privacy-safe middleware

**Files:**
- Create: `Server/internal/httpapi/router.go`
- Create: `Server/internal/httpapi/middleware.go`
- Create: `Server/internal/httpapi/operational.go`
- Create: `Server/internal/httpapi/router_test.go`
- Create: `Server/internal/probe/probe.go`
- Create: `Server/internal/probe/probe_test.go`
- Create: `Server/cmd/baby-tracker-server/main.go`

**Interfaces:**
- Consumes: `config.Config`; `type Readiness interface { Check(context.Context) error }`
- Produces: `httpapi.New(Options) http.Handler`; `httpapi.Routes() []Route`; `GET /healthz`; `GET /readyz`; `GET /v1/server`; `httpapi.Run(ctx, *http.Server) error`; `baby-tracker-server probe --url <health-url>`

- [ ] **Step 1: Write failing handler tests**

Use `httptest` with a fake readiness checker. Assert `/healthz` is always `200 {"status":"ok"}`, `/readyz` is `200` when the checker succeeds and `503` with stable code `not_ready` when it fails, and `/v1/server` returns only `instance_id`, `recovery_epoch`, `canonical_origin`, `api_versions:["v1"]`, and `capabilities:[]`. Send `Authorization`, `Cookie`, and a JSON body containing a baby name; capture `slog` JSON and assert none of those values appears while `request_id`, method, normalized route, status, and duration do.

- [ ] **Step 2: Verify RED**

Run: `rtk go test ./internal/httpapi -v` from `Server/`\
Expected: FAIL because the package and `New` do not exist.

- [ ] **Step 3: Implement router, middleware, and process wiring**

Use Go's method-aware `http.ServeMux`. `Options` contains `Logger *slog.Logger`, `Readiness Readiness`, `InstanceID string`, `RecoveryEpoch uint64`, and `CanonicalOrigin string`; `Route` contains `Method` and `Pattern`. `Routes` is the single registration list later compared with OpenAPI. Generate or validate an inbound `X-Request-ID` as 32 lowercase hex characters, return it in the response, recover panics as the same versioned JSON error envelope, cap request bodies at 1 MiB, set `MaxHeaderBytes` to 1 MiB, and log only the allowlisted fields. `Run` must treat `http.ErrServerClosed` as success and on context cancellation call `Shutdown` with a 10-second child timeout. Main loads config, creates a JSON logger, wires a temporary readiness checker that reports `database_not_configured`, and starts with 5-second read-header, 15-second request-read, 30-second write, and 60-second idle timeouts. The `probe` subcommand uses a dedicated client with a 2-second total timeout, requires HTTP 200, reads at most 4 KiB, and never follows redirects; its tests use `httptest` to pin success, timeout, redirect rejection, oversized response, and non-200 behavior.

- [ ] **Step 4: Verify GREEN**

Run: `rtk go test ./internal/httpapi ./cmd/baby-tracker-server -v && rtk go vet ./...`\
Expected: PASS with no log output from tests and no vet findings.

- [ ] **Step 5: Commit**

```bash
rtk git add Server/internal/httpapi Server/cmd/baby-tracker-server
rtk git commit -m "feat(server): add operational HTTP endpoints"
```

---

### Task 3: Explicit PostgreSQL migrations and readiness

**Files:**
- Create: `Server/internal/database/pool.go`
- Create: `Server/internal/migrate/migrate.go`
- Create: `Server/internal/migrate/status.go`
- Create: `Server/internal/migrate/migrate_test.go`
- Create: `Server/migrations/embed.go`
- Create: `Server/migrations/000001_server_state.sql`
- Create: `Server/cmd/baby-tracker-admin/main.go`
- Modify: `Server/cmd/baby-tracker-server/main.go`

**Interfaces:**
- Consumes: `BT_DATABASE_URL`; SQL files embedded from `Server/migrations`
- Produces: `database.Open(ctx, url) (*pgxpool.Pool, error)`; `migrate.New(source fs.FS) *Runner`; `(*Runner).Apply(ctx, pool) error`; `(*Runner).Status(ctx, pool) (Status, error)`; admin command `migrate`; readiness requiring DB ping and `Current == Required`

- [ ] **Step 1: Write a failing PostgreSQL integration test**

The test reads `BABY_TRACKER_TEST_DATABASE_URL`; when absent it skips with that exact reason. It uses an injected `fstest.MapFS` containing exactly migration 1, so later production migrations cannot invalidate the expectation. Against a clean database it asserts `Status` reports required version 1/current 0, `Apply` creates `schema_migrations` and records checksum/version once, a second `Apply` is idempotent, and changing the applied migration's bytes through a second injected filesystem returns `migration_checksum_mismatch`.

- [ ] **Step 2: Verify RED**

Run with a temporary PostgreSQL 17 container and exported test URL: `rtk go test ./internal/migrate -v`\
Expected: FAIL because `Apply` and `Status` do not exist.

- [ ] **Step 3: Implement the migrator and CLI**

Package `migrations` embeds its adjacent `*.sql` files and exposes a read-only `fs.FS`; `internal/migrate` takes a PostgreSQL advisory lock; creates `schema_migrations(version bigint primary key, name text not null, sha256 text not null, applied_at timestamptz not null default now())`; rejects gaps, changed checksums, and a database newer than the binary; and runs each unapplied file in its own transaction. Migration 1 creates only server identity/recovery metadata. Add pgx v5.11.0 in this task. `baby-tracker-admin migrate` is the only write command. Wire server readiness to a composite checker for database ping and migration equality; authenticated routes do not exist yet, and the next plan adds recovery-quarantine and ledger-head gates before adding them.

- [ ] **Step 4: Verify GREEN and race safety**

Run: `rtk go test -race ./internal/database ./internal/migrate ./internal/httpapi ./cmd/... -v`\
Expected: PASS. Start two `Apply` calls concurrently in the test and assert exactly one applied row.

- [ ] **Step 5: Commit**

```bash
rtk git add Server/internal/database Server/internal/migrate Server/migrations Server/cmd
rtk git commit -m "feat(server): add explicit PostgreSQL migrations"
```

---

### Task 4: Tenant schema and forced row-level security

**Files:**
- Create: `Server/migrations/000002_tenancy.sql`
- Create: `Server/internal/tenant/context.go`
- Create: `Server/internal/tenant/context_test.go`
- Create: `Server/deploy/postgres/init-roles.sql`

**Interfaces:**
- Consumes: a pgx transaction opened as the runtime database role
- Produces: `tenant.SetLocal(ctx, tx, Context{AccountID, FamilyID}) error`; SQL helper functions `app.current_account_id()` and `app.current_family_id()`; default-deny RLS foundation

- [ ] **Step 1: Extend the integration test with two isolated families**

As the migration owner create opaque UUID accounts, families, and memberships. As the runtime role, prove a query before `SetLocal` returns zero rows, family A sees only A, then release/reacquire from a one-connection pool and prove family B sees only B. Attempt an insert whose `family_id` differs from the transaction-local context and assert PostgreSQL rejects it. `tenant.Context` contains `AccountID string` and `FamilyID string`; `SetLocal` accepts `pgx.Tx`.

- [ ] **Step 2: Verify RED**

Run: `rtk go test ./internal/tenant -v` with `BABY_TRACKER_TEST_DATABASE_URL` and owner/runtime credentials.\
Expected: FAIL because the schema and `SetLocal` do not exist.

- [ ] **Step 3: Implement the minimum tenant foundation**

The operator/bootstrap SQL creates separate no-login group roles `baby_tracker_runtime` and `baby_tracker_maintenance`; the application migration never assumes managed PostgreSQL permits role creation. Migration 2 creates UUID-backed `accounts`, `families`, and `memberships`, exactly-one-owner enforcement through a deferred constraint trigger, transaction-local lookup functions using `current_setting(..., true)`, RLS policies on every family-owned table, and `ALTER TABLE ... FORCE ROW LEVEL SECURITY`. The owner role grants only explicit DML to runtime; runtime gets no DDL, role-membership, or bypass-RLS capability. `SetLocal` rejects zero UUIDs and calls `set_config` with the local flag `true` for both values.

- [ ] **Step 4: Verify GREEN and pooled isolation**

Run: `rtk go test -race ./internal/tenant ./internal/migrate -v`\
Expected: PASS, including the one-connection cross-family reuse case.

- [ ] **Step 5: Commit**

```bash
rtk git add Server/migrations/000002_tenancy.sql Server/internal/tenant Server/deploy/postgres/init-roles.sql
rtk git commit -m "feat(server): enforce tenant isolation in PostgreSQL"
```

---

### Task 5: OpenAPI 3.1 foundation and attachment-ready wire types

**Files:**
- Create: `Server/internal/protocol/types.go`
- Create: `Server/internal/protocol/types_test.go`
- Create: `Server/api/openapi.yaml`
- Create: `Server/api/fixtures/error-not-ready.json`
- Create: `Server/api/fixtures/server-info.json`
- Create: `Server/api/fixtures/attachment-metadata.json`
- Modify: `Server/internal/httpapi/operational.go`

**Interfaces:**
- Produces: `protocol.ErrorResponse`, `protocol.Cursor`, `protocol.ServerInfo`, `protocol.AttachmentMetadata`, `protocol.AttachmentManifest`; OpenAPI schemas for the three current GET routes and future metadata only

- [ ] **Step 1: Write failing fixture round-trip tests**

Decode each fixture with `json.Decoder.DisallowUnknownFields`, re-encode it, and compare semantic JSON. Assert `AttachmentMetadata` has ID, family ID, creator account ID, media type, byte size, SHA-256, lifecycle, and exactly one typed link (`baby_id` xor `record_id`), while containing no byte payload, bucket, object key, upload URL, or download URL. Assert cursor JSON contains `server_instance_id`, positive `recovery_epoch`, `family_id`, and non-negative `sequence`.

- [ ] **Step 2: Verify RED**

Run: `rtk go test ./internal/protocol -v`\
Expected: FAIL because protocol types and fixtures do not exist.

- [ ] **Step 3: Implement types, fixtures, and contract**

Use string UUIDs at the wire boundary with explicit `Validate` methods; timestamps are RFC 3339 strings and birth dates (declared for future sync schemas) use `format: date`. OpenAPI declares `openapi: 3.1.0`, HTTPS server variables, stable JSON error envelopes, no attachment path, and only `/healthz`, `/readyz`, `/v1/server`. Make operational handlers encode the protocol structs rather than private duplicates.

- [ ] **Step 4: Verify GREEN and contract-route agreement**

Run: `rtk go test ./internal/protocol ./internal/httpapi -v`. The protocol test parses `api/openapi.yaml` with `gopkg.in/yaml.v3` v3.0.1 and asserts the exact documented method/path set equals the router's exported route descriptors.\
Expected: PASS; no attachment endpoint is present.

- [ ] **Step 5: Commit**

```bash
rtk git add Server/internal/protocol Server/internal/httpapi Server/api Server/go.mod Server/go.sum
rtk git commit -m "feat(server): define the versioned protocol foundation"
```

---

### Task 6: Pinned non-root image, Compose stack, and smoke proof

**Files:**
- Create: `Server/Dockerfile`
- Create: `Server/.dockerignore`
- Create: `Server/compose.yaml`
- Create: `Server/README.md`
- Create: `Server/scripts/smoke.sh`
- Modify: `Makefile`

**Interfaces:**
- Consumes: Docker BuildKit and Compose; configuration from Tasks 1–5
- Produces: `make server-test`, `make server-smoke`, `make server-build`; a non-root multi-architecture-ready service image

- [ ] **Step 1: Run failing deployment checks**

Run: `rtk docker compose -f Server/compose.yaml config` and `rtk docker build -f Server/Dockerfile Server`\
Expected: both FAIL because the files do not exist.

- [ ] **Step 2: Create pinned deployment assets**

Use builder `golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1`, runtime `gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3`, and Compose PostgreSQL `postgres:17.6-alpine@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94`. Build both binaries with `CGO_ENABLED=0`, copy no shell or package manager to runtime, run as distroless nonroot, expose 8080, and use the binary's `probe` subcommand for `HEALTHCHECK` rather than curl.

Compose declares separate database/migration/application services, mounts the role-bootstrap SQL into PostgreSQL's one-time init directory, uses named database/ledger/head volumes, binds the API to loopback only, and passes secrets through files with a development-only origin `http://localhost:8080`. It never embeds Apple private keys. `smoke.sh` creates a unique Compose project, starts the database and application without running the migration service, proves health is `200` while readiness is `503`, runs the one-shot migration service, then proves readiness is `200` and the server identity matches. A trap always tears down its exact project and volumes.

- [ ] **Step 3: Add documented verification targets**

`server-test` runs `cd Server && go test -race ./...`; `server-build` builds `linux/amd64` and `linux/arm64` with Buildx without pushing; `server-smoke` runs the exact smoke script. README documents configuration names without example secrets, explicit migration order, TLS responsibility, the two independent security-state volumes, coordinated backups, and that local smoke evidence is not production evidence.

- [ ] **Step 4: Verify GREEN**

Run: `rtk make server-test && rtk make server-smoke` from the repository root.\
Expected: all Go tests PASS under the race detector; the container smoke reaches healthy and ready states, observes the intentional not-ready state, and leaves no project containers or volumes.

Run: `rtk make test`\
Expected: the existing 233 Swift package tests still PASS with the literal-count guard.

- [ ] **Step 5: Commit**

```bash
rtk git add Server/Dockerfile Server/.dockerignore Server/compose.yaml Server/README.md Server/scripts Makefile
rtk git commit -m "build(server): add pinned container deployment"
```

---

## Plan Completion Gate

After Task 6, run from the repository root:

```bash
rtk make server-test
rtk make server-smoke
rtk make test
rtk git diff --check main...HEAD
```

Then dispatch a most-capable whole-branch reviewer against this plan, its spec, all SDD ledger rulings, and `main...HEAD`. This slice is complete only when that review has no open Critical or Important findings. Do not claim hosted production readiness: this plan proves only the local foundation and container path. The next plan starts Apple identity, sessions, family membership, invitations, entitlements, and the authoritative security ledger/head implementation.
