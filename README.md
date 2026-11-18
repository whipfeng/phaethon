# phaethon

L4 代理与 Mesh / P2P 覆盖网络工具（Go）。支持正向代理、反向连接、规则路由、代理组与订阅，
也支持把多台节点组成 mesh，提供 TUN、DNS、内置 SSH、Web 控制台和签名包自升级。

当前发布：`v0.8.2`（构建版本由 `git describe --tags --match 'v*'` 生成，例如
`v0.8.2-N-g<hash>`）。

> ### 安全须知
>
> **禁止向本仓库提交任何敏感信息**，包括但不限于：
>
> - 私钥、证书包（`.p12` / `.pfx` / `.key` / `.jks`）
> - 代理密码、Token、API Key、SSH 私钥
> - 公网/内网 IP、SSH 端口、用户名、内网域名与网络拓扑
> - 运行时数据库（`data/`，含代理密码与私钥）
>
> 配置请通过 Web 控制台或 Admin API 写入本地数据库；敏感文件仅保存在本地
> `archive/`（已忽略）或部署环境，不进入 Git。`.gitignore` 已覆盖
> `config.yaml`、`*.p12/*.pfx/*.jks/*.key`、`data/`、`archive/`、`scripts/.env`、
> `scripts/signing.key` 等路径。

---

## 核心能力

- **代理协议**：SOCKS5 / Trojan / h_tunnel / HTTP / HTTPS / Direct / Reverse 入站；
  DIRECT / SOCKS5 / Trojan / h_tunnel / VLESS / Hysteria2 / Shadowsocks / SSH / HTTP 出站。
- **规则与调度**：DOMAIN-SUFFIX / IP-CIDR / MATCH 规则，`#MAPPING` 限定；
  select / load-balance / best 代理组；订阅拉取、正则过滤、健康检查。
- **Mesh / P2P**：节点以 `nodeID` 组网，自动拓扑与路由；P2P 会话可跑在
  direct TCP、Trojan、h_tunnel（HTTP 长轮询 / WebSocket）等载体上；支持 UDP 直连 leg
  （`p2p-udp`）、多 lane、按流亲和与 per-carrier writer；UDP leg 复用所属代理链路的
  自身凭据做 AEAD，无全局共享密钥。
- **TUN / 旁路网关**：Linux / Windows / macOS 的 TUN 引擎；旁路网关、DHCP、DNS 劫持与
  Fake-IP；Linux 动态 `tunN`（内核分配）与 fd 生命周期管理；链路健康循环。
- **管理面**：Web 控制台（mTLS 或密码）、Admin API、内置 SSH（`<nodeid>.phn:22`）、
  内置 console（443 / 39999）。
- **升级**：签名的 `.pkg` 上传 → 激活 → watchdog 稳定路径自替换 → ready + 版本校验；
  失败自动回滚 `.old`，不再使用版本化 worker 路径。

---

## 快速开始

```bash
# 编译当前平台
go build -o phaethon .

# 运行（首次会初始化 data/phaethon.db）
./phaethon
```

首次运行（交互式终端）会询问 Admin 端口（默认 `39999`）、是否启用认证、Mesh node-id；
非交互启动使用默认值自动初始化。配置全部持久化在 `data/phaethon.db`，
**没有 YAML 配置文件**。

初始化后：

- Web 控制台：`http://<host>:39999`（生产建议配置 mTLS/认证）
- Mesh 内置 console：`https://<nodeid>.phn/`
- Mesh 内置 SSH：`ssh <nodeid>.phn`

更完整的入门说明见 `docs/`（特别是 `docs/specs/`、`docs/plans/` 和 `docs/runbooks/`）。

---

## 架构概览

```
入站 (socks5/trojan/h_tunnel/http/https/direct/reverse)
        │
        ▼
  规则匹配 + 代理组/订阅 ──► 出站拨号（链式代理 / DIRECT / SSH / 反向）
        │
        ├──► TUN / 旁路网关（DHCP、DNS 劫持、Fake-IP）
        │
        └──► Mesh / P2P（nodeID 组网、拓扑 gossip、链路选择、lane/stripe）
                    │
                    └──► 载体：direct TCP / Trojan / h_tunnel HTTP / WS / UDP direct leg
```

主要模块：

| 目录 | 职责 |
|---|---|
| `admin/` | Web 控制台、Admin API、mTLS、包上传/激活 |
| `handler/` | 入站连接分发、内置 SSH / console |
| `mesh/` | 拓扑、路由选择、DNS 劫持/Fake-IP、TUN 网络栈、链路质量 |
| `p2p/` | P2P 会话、DRR/流队列、lane/stripe、UDP leg、载体重连 |
| `frame/` | 帧协议、载体内核 backlog/pacing、加密、TCP_INFO 统计 |
| `tun/` | Linux/Windows/macOS TUN 引擎、路由/iptables/DHCP/健康循环 |
| `server/`、`dialer/`、`reverse/` | 入站服务器、出站拨号、反向连接 |
| `pkg/upgrade/`、`pkg/signing/` | 自替换升级、签名包 |
| `config/`、`db/` | 配置模型、规则匹配、嵌入式数据库 |

---

## Mesh / P2P

- **节点地址**：mesh CIDR 默认 `100.0.0.0/8`；每个节点分配一个 `/16`：
  VIP `.1`（mesh 路由/NAT 源）、hostIP `.2`（TUN）、GIP `.3`（netstack/DNS）、EIP `.4`（IPIP）。
- **拓扑**：gossip 传播直连邻居与链路质量（srtt / loss / TCP 重传率 / cost）；
  路由按代价选择；支持静态路由、静态域名路由、通告路由。
- **载体**：P2P 会话可复用已配置代理作为载体（direct TCP、Trojan、h_tunnel 等）；
  h_tunnel 在支持时默认尝试 WebSocket，失败回退 HTTP 长轮询。
- **UDP direct leg**：代理开启 `p2p-udp` 后，主动端直接向对端 UDP 端口发送；
  leg 复用该代理链路自身的凭据做 AEAD（h_tunnel/Trojan 用其密码，SOCKS5 配置了密码
  则复用、否则明文）；不可达时回退 TCP。
- **多 lane**：按内层 5 元组做流亲和（避免同一流跨路径重排）；lane 全队列丢弃而不是阻塞
  主写循环；primary 使用独立写协程 + 有界队列，避免 primary 丢包恢复阻塞健康 lane。
- **DNS / 域名路由**：mesh DNS 负责 `<nodeid>.phn` 与域名路由；Fake-IP 池按节点子网分配；
  域名路由支持多 owner 与静态配置。
- **可观测性**：拓扑展示 srtt / loss / TCP retrans / cost；`/api/p2p` 提供 leg 计数、
  lane/qdepth 诊断；控制台只展示当前运行时的真实名字（如 `tunN`），不写死设备名。

详细设计见：

- `docs/plans/mesh-full-topology-design.md`
- `docs/plans/mesh-multilink-stripe-design.md`
- `docs/plans/p2p-architecture-decision.md`
- `docs/specs/protocol_spec.md`

---

## TUN / 旁路网关

- **设备命名**：Linux 使用内核分配的 `tunN`（`TUNSETIFF "tun%d"`，并记录 `state.TunName`）；
  Windows 使用 Wintun 适配器；macOS 使用 `utunN`。
- **生命周期**：Linux/macOS 的 TUN 接口由 fd 生命周期控制（无 `IFF_PERSIST`，`Close` 即销毁）；
  fd 设置为 `CLOEXEC`，不会被内置 SSH/子进程继承，避免残留 `tunN` 与路由错位。
- **旁路网关**：可选 bypass gateway，配合 DHCP 与 DNS 劫持；启动自检会恢复 FORWARD 规则；
  路由配置使用 `RouteReplace`，异常重启不会因 `file exists` 失败。
- **健康循环**：运行期检测 TUN 被外部删除/降级；按 operator intent 恢复；TUN 失败时 mesh 保持运行。

---

## 管理面 / 内置 SSH / 自升级

- **Web 控制台**：`0.0.0.0:39999`；mesh 内置 console 为 `https://<nodeid>.phn/`（443，
  39999 为兼容别名）。支持 mTLS 客户端证书或用户名/密码；证书/凭据保存在本地。
- **内置 SSH**：`<nodeid>.phn:22`；支持公钥认证、可配置 shell；Windows 下按平台提供
  PTY / 行级模式；公钥在控制台维护。
- **自升级**：控制台上传签名 `.pkg` → 激活；watchdog 每 2s 轮询请求，校验签名/平台，
  暂存 `.new`，停止 worker，`exe→.old`、`.new→exe`，启动并校验版本；失败回滚 `.old`。
  稳定路径始终不变，Windows 防火墙/AV 路径规则无需调整。

---

## 配置

所有配置存放在 `data/phaethon.db`，通过 Web 控制台或 Admin API 修改，保存后生效。
README 不再列 YAML 文件；配置实体的字段结构见 `docs/specs/config_spec.md`。

规则示例（通过控制台/规则页填写）：

```
DOMAIN-SUFFIX,example.com,ProxyGroup
IP-CIDR,10.0.0.0/8,DIRECT
MATCH,DIRECT
```

规则可通过 `#MAPPING_NAME` 后缀限定到特定入站 mapping。代理名可以是具体 proxy、
proxy-group，或内置 `DIRECT` / `REJECT`。

### 入站 / 出站协议（摘要）

| 方向 | 协议 |
|---|---|
| 入站 | SOCKS5、Trojan、h_tunnel、HTTP、HTTPS、Direct、Reverse |
| 出站 | DIRECT、SOCKS5、Trojan、h_tunnel、VLESS、Hysteria2、Shadowsocks、SSH、HTTP |

出站拨号自动跟随 `proxy` 字段构建代理链；mesh/P2P 使用相同代理配置作为可选载体。

---

## 构建

```bash
make all          # linux/windows/windows7/darwin(amd64,arm64)
make linux        # Linux amd64
make windows      # Windows amd64
make windows7     # Windows 7（需要 GO_LEGACY_WIN7 指向兼容工具链）
make darwin-amd64 # macOS Intel
make darwin-arm64 # macOS Apple Silicon
```

版本号规则：`git describe --tags --match 'v*' --always`（只匹配发布 tag，避免实验 tag 污染）。
发布包使用 Ed25519 签名，公钥内置在程序中。

---

## 文档索引

| 目录 | 内容 |
|---|---|
| `docs/specs/` | 配置、协议、核心、TUN、反向连接等规格 |
| `docs/plans/` | 架构与专题设计（mesh、link quality、stripe、升级等） |
| `docs/issues/` | 问题定位、实验数据、修复记录 |
| `docs/runbooks/` | 部署与运维手册（不含敏感信息） |
| `docs/tasks/` | 实施任务与状态 |
| `docs/index.md` | 文档导航 |

---

## 版本与发布

- 当前发布 tag：`v0.8.2`
- 二进制版本由 Git 描述生成；各节点在控制台/`--version` 中可查看
- 发布流程：提交 → 推送 → 打 tag → 构建多平台 → 签名包 → 控制台激活/扩散
- 升级状态：`GET /api/packages/pin` 返回 `current/requested/status/lastError`；
  `DELETE` 取消尚未开始的升级请求

---

## 安全与运维

- 敏感信息只通过控制台/Admin API 写入本地数据库；不要把证书包、私钥、密码、IP 提交到仓库。
- 本地敏感文件放在 `archive/` 或部署目录；`archive/`、`data/`、`config.yaml`、
  `*.p12/*.pfx/*.jks/*.key` 已在 `.gitignore` 中忽略。
- 生产建议启用 Admin mTLS/认证；P2P 会话加密由载体协议（及 UDP leg 的代理凭据）保证；
  SSH/SOCKS5 使用强密码或公钥。
- 部署相关注意事项见 `docs/runbooks/`；不要在其中写入真实 IP/凭据。
- 发现安全问题请通过内部渠道反馈，不要在公开 issue 中贴出敏感信息。
