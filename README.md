# sysgate

**System Gateway For VM** — a fast gVisor sandbox gateway.

sysgate wraps gVisor (`runsc`) sandboxes in a low-latency HTTP gateway: a warm
pool keeps sandboxes ready, so requests are served immediately instead of
waiting for a cold boot.

## Why it's fast

| Technique | Effect |
|-----------|--------|
| Warm sandbox pool | `POST /v1/sandboxes` takes O(1) from the pool, skipping runsc cold start |
| Async refill | Refills immediately after checkout; falls back to on-demand creation when empty |
| Lean async stack | tokio + axum + serde; no GC, no runtime; single release binary |
| Direct runsc | No extra proxy hop; `runsc exec` goes straight to the sandbox |
| Low-allocation path | keep-alive, minimal middleware, atomic counters |

## Architecture

```
client ──HTTP──> gateway ──> warm pool ──> runsc (gVisor) sandboxes
                    │                          │
                    └── TTL reaper             └── exec / kill / delete
```

- `src/gateway.rs` — HTTP API, TTL reaper, Prometheus metrics
- `src/pool.rs` — warm pool (mpsc queue + background refill task)
- `src/sandbox/mod.rs` — Driver abstraction
- `src/sandbox/runsc.rs` — gVisor driver (OCI bundle + runsc CLI)
- `src/sandbox/fake.rs` — in-memory driver for local development and tests

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for details.

## Quick start

Run locally with the fake driver (no gVisor required):

```bash
cargo run
curl -s -X POST localhost:8080/v1/sandboxes
```

gVisor mode:

```bash
# Install runsc: https://gvisor.dev/docs/user_guide/install/
export SYSGATE_DRIVER=runsc
export SYSGATE_ROOTFS=/srv/busybox-rootfs   # must contain /bin/sh and sleep
export SYSGATE_POOL_SIZE=8
cargo run --release
```

The gVisor release tarball contains `runsc` plus sidecar binaries under
`gvisor-bin/`. Install both, otherwise sandbox creation fails with
`sidecar "gvisor_sentry" not usable`:

```bash
curl -fsSLO https://github.com/google/gvisor/releases/download/release-20261026.0/gvisor-x86_64.tar.bz2
tar xjf gvisor-x86_64.tar.bz2
sudo install -m 0755 runsc /usr/local/bin/runsc
sudo install -d /usr/local/bin/gvisor-bin
sudo install -m 0755 gvisor-bin/* /usr/local/bin/gvisor-bin/
```

Verify that workloads really run on gVisor:

```bash
ID=$(curl -s -X POST localhost:8080/v1/sandboxes | jq -r .id)
curl -s -X POST localhost:8080/v1/sandboxes/$ID/exec \
  -d '{"argv":["cat","/proc/version"]}'
# {"stdout":"Linux version 4.19.0-gvisor ...","stderr":"","exit_code":0,...}
```

## API

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/sandboxes` | Create a sandbox (takes a warm one when available) |
| GET | `/v1/sandboxes` | List active sandboxes |
| GET | `/v1/sandboxes/{id}` | Get a sandbox |
| DELETE | `/v1/sandboxes/{id}` | Destroy a sandbox |
| POST | `/v1/sandboxes/{id}/exec` | Execute a command inside a sandbox |
| GET | `/healthz` | Health check |
| GET | `/metrics` | Prometheus metrics |

```bash
ID=$(curl -s -X POST localhost:8080/v1/sandboxes | jq -r .id)
curl -s -X POST localhost:8080/v1/sandboxes/$ID/exec \
  -d '{"argv":["echo","hello"]}'
# {"stdout":"hello\n","stderr":"","exit_code":0,"duration_ms":3}
```

## Configuration

| Environment variable | Default | Description |
|----------------------|---------|-------------|
| `SYSGATE_LISTEN` | `:8080` | Listen address |
| `SYSGATE_DRIVER` | `fake` | `fake` or `runsc` |
| `SYSGATE_RUNSC_BIN` | `runsc` | Path to the runsc binary |
| `SYSGATE_ROOT` | `/var/lib/sysgate` | State directory |
| `SYSGATE_ROOTFS` | — | Sandbox rootfs (required for runsc) |
| `SYSGATE_NETWORK` | `none` | runsc network mode |
| `SYSGATE_POOL_SIZE` | `4` | Warm pool size |
| `SYSGATE_MAX_SANDBOXES` | `256` | Active sandbox limit |
| `SYSGATE_DEFAULT_TTL` | `10m` | Default sandbox TTL |
| `SYSGATE_EXEC_TIMEOUT` | `30s` | Default exec timeout |

## Development

```bash
cargo test
cargo clippy --all-targets -- -D warnings
cargo fmt
cargo build --release
```

## Roadmap

- [ ] runsc checkpoint/restore snapshots for millisecond cold starts
- [ ] OCI image pull and rootfs caching
- [ ] Per-sandbox network policy / vsock channels
- [ ] Multi-node scheduling and pool watermarks
- [ ] OpenTelemetry tracing

## License

Apache-2.0, matching gVisor.

sysgate drives gVisor sandboxes through runsc. gVisor is developed by The
gVisor Authors and licensed under the Apache License 2.0. See [NOTICE](NOTICE).
