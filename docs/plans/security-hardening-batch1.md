# 安全止血批次（Batch 1）设计

> 版本: 0.1.0
> 日期: 2026-11-09
> 状态: DRAFT
> 负责人: Phaethon Dev

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-11-09 | 初始版本：由全仓审计 D3/D4/D12/D13/D23 收敛 | Dev |

## 1. 目标

按 `docs/issues/full-code-audit-20261109.md` 的决策记录，完成第一批安全相关改造：

- 包分发从“自动发布/自动升级/自动转发”改为“只提醒，用户决定”（D3）
- 通用数据库接口默认关闭，开启后只读 + 白名单（D4）
- 入站映射支持配置监听地址；空密码 socks5 启动告警（D12）
- 反向注册端口强制范围与下限；动态监听跟随监听地址（D13）
- 后台会话与凭据安全：密码哈希、独立会话密钥、Cookie 安全、改密失效、接口脱敏（D23）
- 顺带修两个确定性 bug（审计“直接修复清单” #3、#13）

## 2. 非目标

- 不改 mTLS 架构（D5 维持现状）
- 不做代理凭据加密（D31 暂缓）
- 不重构健康检查、数据面内存、TUN 生命周期（后续批次）

## 3. 设计与落点

### 3.1 包分发（D3）

- **目录**：接收/下载的包写入 `data/packages-pending/`；已启用的包留在 `data/packages/`。
- **watchdog**：只扫描 `data/packages/`（以及 pin 文件），绝不读取 pending。
- **pin**：`data/state/worker-pin` 记录已启用版本；watchdog 优先使用 pin 版本，找不到回退 last-good。
- **接收**：`POST /api/packages/receive` 验签后只落 pending，`Published=false`；删除 `go checkForNewerVersion` 与 `go DistributePackage`。
- **启用**：新增 admin API「启用版本」：把 pending 包移入 `data/packages/`，写 pin；提供 `restart=now|later`。
- **分发**：改为管理员手动触发（现有 `DistributePackage` 仅由显式操作调用）。
- **限制**：receive/upload 均使用 `http.MaxBytesReader`（默认 256MB）。

### 3.2 通用数据库接口（D4）

- 新增启动参数 `--enable-debug-api`（或环境变量 `PHAETHON_DEBUG_API=1`），默认关闭。
- 关闭时 `/api/db/**`、pprof、stall-dump 均返回 404。
- 开启后：
  - 仅允许 GET；
  - bucket 白名单：proxies、proxy-groups、subscriptions、subscription-nodes、rules、mappings、resolvers、reverse-configs、bindings、port-range-config、tun-config、mesh-config、versions、logs、webhooks；
  - 列表返回条数上限（如 1000）。

### 3.3 监听地址与注册端口（D12/D13）

- `config.Mapping` 增加 `listen_addr`（默认 `0.0.0.0`）；`startTCP`/`startTLS` 使用它。
- socks5 密码为空：启动时 `WARN` 明确提示“无认证开放代理”。
- 反向注册：默认端口范围 `20000-39999`；拒绝 < 1024；动态 listener 使用 `listen_addr`。

### 3.4 后台会话与凭据（D23）

- 密码改为 bcrypt 哈希存储；登录使用恒定时间比较。不做旧明文自动迁移。
- 会话签名密钥独立生成并持久化到 `data/state/session.key`（0600），不再复用 admin token。
- Cookie 增加 `Secure`；有效期缩短；改密时轮换会话密钥（旧会话立即失效）。
- `/api/config/admin` 等接口脱敏：密码、token、私钥不回显。

## 4. 风险与兼容

- 包目录拆分后，旧版本遗留的 `data/packages/*.pkg` 仍可被 watchdog 使用；不做强制迁移。
- 密码哈希上线后旧明文密码无法登录，由管理员自行重置（D23 约定）。
- `listen_addr` 默认保持 `0.0.0.0`，不改变现有行为。

## 5. 验收

- 接收包不再触发升级/转发；pending 包不影响 watchdog 选择。
- 默认启动时 `/api/db/**`、pprof 不可访问；开启后只读且白名单生效。
- 映射可配置监听地址；空密码 socks5 有醒目告警。
- 注册端口拒绝 <1024，未配置范围时使用 20000-39999。
- 密码以哈希存储；改密后旧会话失效；配置接口不返回明文凭据。
- `go build .` 与三平台交叉编译通过；相关包测试通过。
