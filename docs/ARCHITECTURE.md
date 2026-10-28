# sysgate architecture

## Goals

Turn gVisor sandboxes into low-latency, programmable gateway resources:

- **Fast**: no sandbox boot cost on the request path (warm pool).
- **Small**: one static binary with a lean dependency set.
- **Isolated**: every sandbox runs its own gVisor user-space kernel instead of
  sharing the host kernel attack surface.

## Components

```
src/main.rs            process entry: config, driver selection, HTTP server, graceful shutdown
src/gateway.rs         HTTP API, active sandbox table, TTL reaper, metrics
src/pool.rs            warm pool: mpsc queue + background refill task
src/sandbox/mod.rs     Driver enum and domain types
src/sandbox/runsc.rs   gVisor driver (OCI bundle + runsc CLI)
src/sandbox/fake.rs    in-memory driver for tests and demos
```

## Request path (fast path)

1. `POST /v1/sandboxes`
2. The gateway checks the active sandbox limit.
3. `pool.acquire` pops a pre-booted sandbox from the warm channel (microseconds).
4. The background refill task immediately creates a replacement.
5. The gateway returns 201 with sandbox metadata.

When the pool is empty, `acquire` takes the slow path and creates a sandbox
synchronously (runsc boot). This preserves correctness at higher latency; watch
`sysgate_pool_misses_total` and raise `SYSGATE_POOL_SIZE` when it grows.

## gVisor lifecycle

The `runsc` driver generates an OCI bundle per sandbox:

```
<SYSGATE_ROOT>/sandboxes/<id>/bundle/
└── config.json   # minimal OCI runtime spec
```

The rootfs is referenced by `SYSGATE_ROOTFS` and is not copied into the bundle.

- **Create**: `runsc --root <root> run -d --bundle <bundle> --network <mode> <id>`.
  The init process is `sh -c 'while :; do sleep 3600; done'`, keeping the
  sandbox alive.
- **Exec**: `runsc --root <root> exec [--cwd] [--env ...] <id> <argv...>`.
  Timeouts are enforced by `tokio::time::timeout`; `kill_on_drop` terminates the
  client process when the future is cancelled.
- **Destroy**: `runsc kill <id> KILL` followed by `runsc delete -force <id>`,
  then the local bundle directory is removed.

Sandbox IDs come from `crypto`-grade randomness (`uuid` v4, `sg-` prefix) and
are re-validated against a lowercase alphanumeric-plus-dash allowlist in the
driver, preventing path traversal.

## TTL reaping

A tokio interval task scans the active table, removes expired entries, and
destroys their sandboxes. The default TTL is 10 minutes; callers can override it
with `ttl_seconds` (`0` means "use the server default").

## Concurrency model

- Active table: `tokio::sync::RwLock<HashMap<..>>`.
- Warm pool: bounded `mpsc` channel whose capacity is the pool size; a single
  refill task avoids creation storms.
- Counters: atomics, so `/metrics` never blocks request handling.
- Graceful shutdown: HTTP shutdown → reaper aborted → warm sandboxes destroyed →
  process exits.

## Security boundaries

- Each sandbox is isolated by the gVisor user-space kernel; syscalls do not
  reach the host kernel directly.
- `--network none` by default; networking is opt-in.
- The API has no authentication of its own and is meant to sit behind a trusted
  network boundary or an authenticating proxy.

## Roadmap

1. **Snapshots**: runsc checkpoint/restore, turning warm-up into snapshot restore.
2. **Images**: OCI registry pulls plus a content-addressable rootfs cache.
3. **Network**: per-sandbox policy and vsock data channels.
4. **Multi-node**: scheduler driven by pool watermark metrics.
5. **Observability**: OpenTelemetry traces and structured audit logs.
