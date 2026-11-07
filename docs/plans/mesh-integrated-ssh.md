# mesh 内建 SSH 服务 — `ssh nodeid.phn` 直达节点（2026-11-07）

| | |
|---|---|
| 状态 | **已实施（2026-11-07）**：分流 + server/ssh.go + db + API + Admin 页区块完成，冒烟测试通过；真机自测（ssh vm.phn / qg.phn）待部署后验证 |
| 关联 | 控制台后端同构机制（ServeConn 分流）；依赖 golang.org/x/crypto（已在 go.mod:12）|

## 一、需求与可行性结论

**需求**：`ssh nodeid.phn` 直达目标节点，交给节点内建的 SSH 服务，全程不经过 OS 网络栈（与控制台后端 443 同机制）；SSH 用 22 端口、控制台用 443 端口区分；认证用公钥，管理页面可粘贴公钥。

**结论：可行，且改动面小**。nodeid.phn 的解析（fakeIP/mesh 子网）、跨节点投递（mesh SendToNode）、目标节点 netstack 内终止（tcp.NewForwarder → ConnectionHandler.HandleTCPConn）整条链路与控制台完全共用，唯一缺口是 **localNodeDomain 分支不分端口**（全部进 admin TLS）+ 节点上没有 SSH server。两处补齐即成。

## 二、现状机制（调研实锤，文件:行号）

- DNS：`nodeid.phn` 由 DNSHijacker 应答（mesh/dns.go:585），本节点 fakeIP 或远端子网 IP；`ConnectionHandler` 收到连接后 `fakeIP.LookupDomain` 反查域名（handler/connection_handler.go:100）。
- 本地识别：`domain == localMeshNodeID+".phn"`（connection_handler.go:120）或 dst 是本地 VIP/子网（135/145）→ `localNodeDomain=true`。
- **分流缺口**：localNodeDomain 早退分支**无视 dstPort**，一律 `adminHandler.ServeConn(conn)`（connection_handler.go:174-186）。今天 `ssh nodeid.phn`（22）会撞进 TLS handler 失败。
- 控制台终止方式：`AdminServer.ServeConn`（admin/admin.go:857）= `tls.Server` 包一层 + `singleConnListener` 喂同一个 http.Server——**单连接直喂 handler，不经 OS 栈**。SSH 照抄这个模式：x/crypto/ssh 的 `ssh.NewServerConn` 直接吃 `net.Conn`（gonet.TCPConn 即可）。
- 存储：bbolt KV（db/db.go:137-204，`Put/Get/ForEach/Delete` + `configBuckets()` 注册 bucket）；TLS 证书已有 loadOrGenerateTLSCert（admin/admin.go:5333）模式可抄（host key 同法存 db）。
- admin UI：embed 模板（admin/embed.go）+ `registerRoutes`（admin/admin.go:1254）+ GET/PUT 同 handler 分支范式（apiLogModules admin/admin.go:5364）。
- 依赖：`golang.org/x/crypto v0.50.0` 已在 go.mod（dialer/ssh.go 客户端用过，server 端 API 同包）；交互 PTY 需新增 `github.com/creack/pty`（unix only）。

## 三、设计

### 3.1 接入分流（改动最小点）

`ConnectionHandler` 增加 `sshHandler`（接口仿 AdminHandler：`ServeConn(net.Conn)`，handler/connection_handler.go:22-24）。localNodeDomain 早退分支内：

```
if dstPort == 22 && h.sshHandler != nil → sshHandler.ServeConn(conn)
else → 维持 adminHandler.ServeConn(conn)   // 443 等行为不变
```

远端节点路径零改动：客户端 OS 栈只到本机 TUN，SYN 经 mesh 到目标节点 netstack，forwarder 回调进 HandleTCPConn，按端口分流。main.go 接线照抄 `connHandler.SetAdminHandler`（main.go:536）加一行 `SetSSHHandler`。

### 3.2 SSH server（新文件 server/ssh.go + ssh_session_unix.go / ssh_session_windows.go）

- 每节点一个 server 实例；host key ed25519，`loadOrGenerateSSHHostKey` 仿 TLS 证书模式存 db（bucket `ssh`，key `host_ed25519`）；指纹（SHA256）导出给 admin 页面展示，用户首次连接比对。
- `ServerConfig{PublicKeyCallback}`：查 authorized keys（内存缓存，db 变更时失效）。**仅公钥，不做密码认证**。
- 会话：
  - `session` channel + `shell`/`exec` 请求 → **拉起系统默认 shell**：
    - Linux：`$SHELL`（空则 `/bin/sh`），creack/pty 全交互，`window-change` 消息转发 `pty.SetWinsize`。
    - Windows（VM/QGW）：`%COMSPEC%`（默认 C:\Windows\system32\cmd.exe）管道模式（行级交互可用，无全屏 TUI）；ConPTY 全交互列二期。
  - 其他 channel（direct-tcpip、tcpip-forward 等）v1 一律拒绝。
- 总开关 `ssh.enabled`（db 配置，默认开？——实施时定，见 §六风险），页面可关。

### 3.3 公钥管理与页面

- db bucket `ssh_keys`：**支持多条公钥**（多台设备/多人各一条），key = 公钥指纹（SHA256），value = `{name, pubkey, created_at}`（name 为备注，便于辨识归属，db/models.go 加模型）。
- API（走现有 authMiddleware，即 admin 登录态才可管理）：
  - `GET /api/ssh-keys`：列表 + host key 指纹 + enabled 状态。
  - `POST /api/ssh-keys`：粘贴一行公钥文本，`ssh.ParseAuthorizedKey` 解析校验、拒绝非法/重复。
  - `DELETE /api/ssh-keys/{fp}`。
  - `PUT /api/ssh-keys/config`：启停开关。
- 页面：**并入现有 ⚙️ Admin 设置页**（admin/templates/admin.html）作为「SSH 服务」区块，不新增导航项；区块内含 enabled 开关 + host key 指纹展示 + 公钥列表 CRUD。CRUD 范式抄页面内现有区块（如 apiMeshDomainSuffixes，admin/admin.go:4460）。
- Bootstrap：第一个公钥由 admin 登录后在页面粘贴（admin 本身已过 mTLS/cookie 认证），无需旁路。

### 3.4 用户侧使用

```
ssh -o StrictHostKeyChecking=accept-new admin@qg.phn
```
（用户名语义：v1 忽略用户名或统一映射到运行用户；页面标注 host key 指纹供比对。）

## 四、实施步骤

1. handler 端口分流 + `SetSSHHandler` 接线（connection_handler.go、main.go，约 2 处）。
2. server/ssh.go：host key 存取、ServerConfig、公钥校验缓存、channel 处理骨架。
3. 会话执行：unix pty 版 + windows 管道版（加 creack/pty 依赖）。
4. db 模型 + API（db/models.go、admin/admin.go 路由与 handler）。
5. 页面：Admin 设置页内新增「SSH 服务」区块（admin.html + app.js CRUD）。
6. 本机构建（版本戳）→ VM 自测（本机即 VM）：`ssh vm.phn`（经 wintun 回环）登录拿 shell；再测跨节点 `ssh qg.phn`。
7. QG 部署验证；JF/QGW 用户确认后滚动（生产纪律）。

## 五、验收

1. `ssh <nodeid>.phn` 从任意 mesh 节点（含跨节点）登录目标节点成功，全程不经 OS 监听端口（节点上 `ss -tlnp` 无 22 监听，连接在 netstack 内终止）。
2. 未授权公钥被拒；删除公钥后会话立即失效（缓存失效）。
3. 443 控制台行为无回归；其它端口经 nodeid.phn 的行为不变。
4. Windows 节点可用（至少 exec/行级 shell），Linux 节点全交互 PTY。
5. 页面 CRUD 全通，host key 指纹展示与 `ssh-keyscan` 实值一致。

## 六、风险与回滚

| 风险 | 缓解 |
|---|---|
| shell = phaethon 进程权限的远程执行面 | 仅公钥认证；enabled 开关；页面显著警告；二期可加 per-key 命令白名单 |
| Windows 交互体验差（无 ConPTY） | v1 明确标注；二期 ConPTY |
| x/crypto/ssh 处理 netstack gonet.Conn 的边界（半关闭、超时） | 与 admin ServeConn 同 transport，已有 soak 验证；自测覆盖大输出/中断连接 |
| 公钥缓存与 db 一致性 | 增删 API 内同步失效缓存，单节点单进程无并发写问题 |

回滚：置 `ssh.enabled=false`（秒级）或退回上次运行版本二进制（prev-running 纪律）。

## 七、二期（本期不做）

- 密码认证 / per-key 授权命令白名单 / 会话录制审计
- SFTP、ssh -L/-R 端口转发（经 ssh 通道复用 mesh 路由）
- Windows ConPTY 全交互
- 公钥 mesh 全局同步（一次配置多节点生效）
