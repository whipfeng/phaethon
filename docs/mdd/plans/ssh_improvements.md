# SSH 代理改进计划

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-08-25

## 背景

当前 `dialer/ssh.go` 实现了基于 SSH `direct-tcpip` 的 TCP 出站代理，支持密码和私钥认证，并通过缓存复用 SSH client。代码可用但缺少测试、超时控制和缓存生命周期管理，认证方式也较为单一。

## 目标

1. 提升 SSH dialer 的健壮性（超时、缓存清理）。
2. 补充单元测试，覆盖认证和连接路径。
3. 扩展认证方式（ssh-agent）。
4. 明确 UDP 支持范围与实现路径。

## 阶段 1: 测试补齐

### Task 1.1: 私有函数可测化
- 将 `createSSHClient` 的依赖（`nextDialer.Dial`、`ssh.NewClientConn`）通过接口注入，或新增 `createSSHClientWithConn(conn net.Conn)` 便于测试。
- 保持现有公共 API 不变。

### Task 1.2: 新增 `dialer/ssh_test.go`
- 测试密码认证：构造 mock SSH server，验证 `SSHDialer.Dial` 能建立 `direct-tcpip` 通道。
- 测试私钥认证：使用内存中的测试密钥对。
- 测试 stale client 清理：模拟首次 `client.Dial` 失败后重试成功。
- 测试无认证方法时返回错误。

### Task 1.3: 运行测试
- `go test ./dialer -run TestSSH`
- 确保无 race（`go test -race ./dialer`）。

## 阶段 2: SSH handshake 超时

### Task 2.1: 为底层连接设置 deadline
- 在 `createSSHClient` 中，对 `conn` 设置 `SetDeadline(time.Now().Add(sshHandshakeTimeout))`，handshake 成功后清除 deadline。
- 建议超时默认值 15s，可通过环境变量或常量调整。

### Task 2.2: 验证超时行为
- 在单元测试中 mock 一个 hang 住的 server，验证 handshake 超时返回错误。

## 阶段 3: SSH client 缓存生命周期

### Task 3.1: 引入带超时的缓存
- 将 `sshClientCache` 从 `map[string]*ssh.Client` 改为带 `lastUsed` 时间戳的结构。
- 新增后台 goroutine 或懒清理：超过 `sshClientIdleTimeout`（建议 5 分钟）未使用的 client 主动 `Close` 并从缓存删除。
- 在 `Dial` 成功时更新 `lastUsed`。

### Task 3.2: 处理并发安全
- 保持 `sshCacheMu` 保护缓存读写。
- 清理逻辑加锁，避免关闭正在被使用的 client。

### Task 3.3: 参考主流做法
- OpenSSH `ControlMaster` 通过持久 socket 复用连接，没有固定 idle 超时，依赖用户配置或进程退出。
- Go 的 `golang.org/x/crypto/ssh` 没有内置连接池，主流代理工具（如 v2ray、clash）通常按配置名缓存并在配置重载时清理。
- 本方案采用“懒清理 + 配置重载时全清”，与主流实现一致。

## 阶段 4: ssh-agent 认证支持

### Task 4.1: 使用 `x/crypto/ssh/agent`
- 在 `createSSHClient` 中，如果 `SSH_AUTH_SOCK` 环境变量存在且用户未配置私钥/密码，尝试连接本地 ssh-agent。
- 使用 `agent.NewClient(conn)` 获取 `agent.Agent`，通过 `ssh.PublicKeysCallback(agent.Signers)` 加入 `sshConf.Auth`。

### Task 4.2: 优先级
- 私钥 > 密码 > ssh-agent。
- 或者让用户显式启用：`use-agent: true`。

### Task 4.3: 验证
- 在支持 ssh-agent 的环境中手动验证。
- 单元测试 mock agent client。

## 阶段 5: UDP 支持分析

### Task 5.1: 明确结论
- SSH 协议本身（`direct-tcpip`）只转发 TCP，**不原生支持 UDP**。
- 要让 UDP 走 SSH 代理，标准做法是在 SSH 隧道内再跑一层 SOCKS5，利用 SOCKS5 的 `UDP ASSOCIATE`：
  - phaethon 在本地开一个 SOCKS5 入口（或复用现有 mapping）。
  - 该 SOCKS5 通过 SSH `direct-tcpip` 连接到远程主机上的某个 SOCKS5 server（或远程 SSH 主机本身运行 socks5）。
  - SOCKS5 server 收到 `UDP ASSOCIATE` 后，在远程主机上监听真实 UDP 端口，并向目标发送真实 UDP 包。
  - 本地到远程 SOCKS5 的 UDP 流量通过 TCP SSH 隧道封装。

### Task 5.2: 澄清“真实 UDP”
- 是的，SOCKS5 最终需要远程 SOCKS5 server 发出真实 UDP。
- 客户端（phaethon 侧）只负责把 UDP 包封装进 SOCKS5 UDP 帧，发给 SOCKS5 relay；relay 负责解封装并发出真实 UDP。
- 所以如果 SOCKS5 server 跑在远程 SSH 主机上，真实 UDP 就是从远程主机发出的。

### Task 5.3: 实现决策
- **本阶段不实现 UDP**：因为需要额外在 SSH 连接内维护一个 SOCKS5 server/relay，复杂度高于现有 `direct-tcpip` 模型。
- 保留为后续可选扩展，当前 README 继续标记 SSH UDP 为 `—`。

## 阶段 6: 文档更新

### Task 6.1: 更新 `docs/mdd/specs/protocol_spec.md`
- 补充 SSH 认证方式说明（password / private-key / ssh-agent）。
- 说明 UDP 不支持及原因。

### Task 6.2: 更新 `README.md`
- SSH 行说明改为“通过 SSH `direct-tcpip` 转发 TCP；支持密码/私钥/ssh-agent”。
- UDP 保持 `—`。

## 验收标准

- `go test ./dialer` 全部通过，新增 SSH 测试覆盖主要路径。
- `go test ./...` 无回归。
- `go build ./...` 成功。
- README 和 protocol_spec.md 准确反映当前能力。
