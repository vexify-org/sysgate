# sysgate 架构

## 目标

把 gVisor 沙箱做成低延迟、可直接编程的网关资源：

- **快**：请求路径上没有沙箱启动开销（预热池）。
- **小**：单个静态二进制，零第三方依赖。
- **稳**：每个沙箱独立 gVisor 内核，用户态隔离，不共享宿主内核攻击面。

## 组件

```
cmd/sysgate          进程入口：配置、驱动选择、HTTP 服务、优雅退出
internal/gateway     HTTP API、活跃沙箱表、TTL 回收、指标
internal/pool        预热池：channel 队列 + 后台补充 goroutine
internal/sandbox     Driver 接口与领域类型
internal/sandbox/runsc   gVisor 驱动（OCI bundle + runsc CLI）
internal/sandbox/fake    内存驱动（测试/演示）
```

## 请求路径（快路径）

1. `POST /v1/sandboxes`
2. gateway 检查活跃上限。
3. `pool.Acquire` 从 `warm` channel 取一个已启动的沙箱（微秒级）。
4. 后台 refill goroutine 立即创建新沙箱补位。
5. 返回 201 + 沙箱元数据。

池空时 `Acquire` 走慢路径：同步创建（runsc 启动）。这保证正确性，但延迟更高；`sysgate_pool_misses_total` 可用于观察并调大 `SYSGATE_POOL_SIZE`。

## gVisor 生命周期

`runsc` 驱动为每个沙箱生成 OCI bundle：

```
<SYSGATE_ROOT>/sandboxes/<id>/bundle/
├── config.json   # 最小 OCI runtime spec
└── (rootfs 由 SYSGATE_ROOTFS 指向，不入 bundle 目录)
```

- **创建**：`runsc --root <root> run -d --bundle <bundle> --network <mode> <id>`，init 进程为 `sh -c 'while :; do sleep 3600; done'`，使沙箱保持存活。
- **执行**：`runsc --root <root> exec [--cwd] [--env ...] <id> <argv...>`，超时由 `exec.CommandContext` 终止。
- **销毁**：`runsc kill <id> KILL` + `runsc delete -force <id>`，并清理本地 bundle 目录。

沙箱 ID 使用 `crypto/rand` 生成（`sg-` + 24 hex），驱动侧再做字符白名单校验，避免路径穿越。

## TTL 回收

gateway 内一个 ticker 周期性扫描活跃表，删除过期条目并销毁沙箱。默认 TTL 10 分钟，可通过请求 `ttl_seconds` 覆盖；`ttl_seconds: 0` 使用服务端默认值。

## 并发模型

- 活跃表：`sync.RWMutex` + map。
- 预热池：有缓冲 channel，容量即池大小；refill 单 goroutine，避免创建风暴。
- 计数器：`sync/atomic`，`/metrics` 读取无锁。
- 优雅退出：HTTP shutdown → gateway reaper 停止 → 池内沙箱销毁 → 驱动关闭。

## 安全边界

- 每个沙箱由 gVisor 用户态内核隔离，系统调用不直接触达宿主内核。
- 默认 `--network none`，不暴露网络；需要时显式切换。
- API 本身不做认证，预期部署在可信内网或前置鉴权网关之后。

## 路线图

1. **快照**：runsc checkpoint/restore，预热改为「恢复快照」，进一步压缩冷启动。
2. **镜像**：OCI registry 拉取 + rootfs 缓存（content-addressable）。
3. **网络**：每沙箱策略、vsock 数据通道。
4. **多节点**：池水位指标驱动的调度器。
5. **可观测**：OpenTelemetry traces + 结构化审计日志。
