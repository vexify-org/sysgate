# sysgate

**System Gateway For VM** — 基于 gVisor 的极速沙箱网关。

sysgate 把 gVisor（runsc）沙箱封装成低延迟 HTTP 网关：预热池里始终有待命沙箱，请求到达时直接取用，而不是现场等待沙箱启动。

## 为什么快

| 手段 | 效果 |
|------|------|
| 预热沙箱池 | `POST /v1/sandboxes` 从池中 O(1) 取用，避开 runsc 冷启动 |
| 异步补池 | 取用后后台立即补充；池空时退化为按需创建 |
| 零第三方依赖 | 仅 Go 标准库，无供应链负担，静态单二进制 |
| 直连 runsc | 不经过额外代理层，`runsc exec` 直达沙箱 |
| 标准库 HTTP | 最小中间件、keep-alive、低分配 |

## 架构

```
client ──HTTP──> gateway ──> warm pool ──> runsc (gVisor) sandboxes
                    │                          │
                    └── TTL reaper             └── exec / kill / delete
```

- `internal/gateway` — HTTP API、TTL 回收、Prometheus 指标
- `internal/pool` — 预热池（channel 队列 + 后台补充）
- `internal/sandbox` — Driver 抽象
- `internal/sandbox/runsc` — gVisor 驱动（OCI bundle + runsc CLI）
- `internal/sandbox/fake` — 内存驱动（本地开发/测试）

详见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 快速开始

本地试跑（fake 驱动，无需 gVisor）：

```bash
go run ./cmd/sysgate
curl -s -X POST localhost:8080/v1/sandboxes
```

gVisor 模式：

```bash
# 安装 runsc: https://gvisor.dev/docs/user_guide/install/
export SYSGATE_DRIVER=runsc
export SYSGATE_ROOTFS=/srv/busybox-rootfs   # 需包含 /bin/sh 与 sleep
export SYSGATE_POOL_SIZE=8
go run ./cmd/sysgate
```

## API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/sandboxes` | 创建沙箱（优先取用预热沙箱） |
| GET | `/v1/sandboxes` | 列出活跃沙箱 |
| GET | `/v1/sandboxes/{id}` | 查询沙箱 |
| DELETE | `/v1/sandboxes/{id}` | 销毁沙箱 |
| POST | `/v1/sandboxes/{id}/exec` | 在沙箱内执行命令 |
| GET | `/healthz` | 健康检查 |
| GET | `/metrics` | Prometheus 指标 |

```bash
ID=$(curl -s -X POST localhost:8080/v1/sandboxes | jq -r .id)
curl -s -X POST localhost:8080/v1/sandboxes/$ID/exec \
  -d '{"argv":["echo","hello"]}'
# {"stdout":"hello\n","stderr":"","exit_code":0,"duration_ms":3}
```

## 配置

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `SYSGATE_LISTEN` | `:8080` | 监听地址 |
| `SYSGATE_DRIVER` | `fake` | `fake` / `runsc` |
| `SYSGATE_RUNSC_BIN` | `runsc` | runsc 路径 |
| `SYSGATE_ROOT` | `/var/lib/sysgate` | 状态目录 |
| `SYSGATE_ROOTFS` | — | 沙箱 rootfs（runsc 必填） |
| `SYSGATE_NETWORK` | `none` | runsc 网络模式 |
| `SYSGATE_POOL_SIZE` | `4` | 预热池大小 |
| `SYSGATE_MAX_SANDBOXES` | `256` | 活跃上限 |
| `SYSGATE_DEFAULT_TTL` | `10m` | 默认 TTL |
| `SYSGATE_EXEC_TIMEOUT` | `30s` | 默认 exec 超时 |

## 开发

```bash
make test
make vet
make build
```

## 路线图

- [ ] runsc checkpoint/restore 快照，把冷启动压到毫秒级
- [ ] OCI 镜像拉取与 rootfs 缓存
- [ ] 每沙箱网络策略 / vsock 通道
- [ ] 多节点调度与池水位控制
- [ ] OpenTelemetry 追踪

## License

Apache-2.0
