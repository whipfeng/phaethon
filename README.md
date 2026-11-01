# phaethon

L4 代理转发工具（Go 版）。支持多种入站/出站协议、规则路由、代理组、订阅、健康检查、反向连接和 TUN 模式。

> ### 安全须知
>
> **禁止向本仓库提交任何敏感信息**，包括但不限于：
>
> - IP 地址（公网 IP、内网 IP）
> - SSH 端口、用户名、密码、密钥
> - 代理密码、Token、API Key
> - 内网域名、网络拓扑
>
> 敏感配置（代理密码、Admin 凭据等）请通过 Web 控制台或 Admin API 写入本地数据库，
> 不要提交到仓库。运维相关信息请参考 `docs/runbooks/` 目录。

## 目录

- [快速开始](#快速开始)
- [新手使用说明](#新手使用说明)
- [项目结构](#项目结构)
- [配置文件](#配置文件)
- [支持的协议](#支持的协议)
- [编译](#编译)
- [运行](#运行)
- [核心模块说明](#核心模块说明)
- [反向连接（Reverse）](#反向连接reverse)
- [TUN 模式](#tun-模式)
- [多环境配置](#多环境配置)

---

## 快速开始

```bash
# 1. 进入目录
cd phaethon

# 2. 编译当前平台
go build -o phaethon .

# 3. 运行
./phaethon
```

首次运行时，程序会在 `data/` 目录创建嵌入式数据库（`data/phaethon.db`）并进入初始化流程：
终端交互模式下会询问 Admin 端口（默认 39999）、是否启用认证、Mesh node-id；
非交互模式（如服务方式启动）直接使用默认值自动初始化。

所有配置（代理、代理组、订阅、规则、端口映射等）都持久化在该数据库中，
通过 Web 控制台或 Admin API 管理，**没有 YAML 配置文件**。

初始化完成后即可：
- 打开 Web 管理面板（默认 `http://<host>:39999`）在 UI 中管理全部配置
- 或通过 Admin API 编程管理（参考 `docs/` 目录）

---

## 新手使用说明

> **phaethon 是什么？**  
> 一个 L4 代理转发工具，把本地服务通过反向通道暴露到公网，也支持正向代理、规则路由、代理组、订阅和 TUN 模式。配置全部保存在本地数据库中，通过 Web 控制台管理，下面介绍最简用法。

### 1. 启动程序

直接运行编译好的二进制：

```bash
./phaethon
```

首次启动会进入初始化（Admin 端口、可选认证、Mesh node-id），完成后配置保存在 `data/phaethon.db`。
Web 管理面板默认监听 `0.0.0.0:39999`。如果端口被占用，启动会失败并在 `data/logs/startup-error.log` 留下原因。

### 2. 添加一个本地 SOCKS5 代理

打开 Web 管理面板 → 端口映射（Mappings）→ 新增：
- 类型：`socks5`
- 端口：`7890`

保存后立即生效，本地即可使用：

```
socks5://127.0.0.1:7890
```

无需任何出站代理节点即可把流量直接转发到目标（默认无规则时走 DIRECT）。

### 3. 反向连接（内网穿透）

使用反向连接前，需要先配置一个能连到注册端的出站代理节点。配置完成后，打开反向连接向导：

```
http://<host>:39999/reverse
```

按向导填写：
- 出站代理：选择已配置的、可到达注册端的代理节点
- 监听模式：选 `Direct`，目标填 `127.0.0.1:8080`（把本地 8080 暴露出去）
- 首选端口：留空让注册端自动分配，或填具体数字

点击 **保存并启用**，等待 1–3 秒：
- 成功：页面显示外部访问地址，例如 `DIRECT://<your-server-ip>:<port>`
- 失败：页面红色提示错误原因（如“端口已被占用”），修改后重新保存即可

### 4. 外部访问

把得到的外部访问地址分享给其他用户，他们即可通过注册端连接到你的本地服务。

### 5. 常见问题

- **端口被占用提示**：首选端口填别的数字，或留空自动分配。
- **同一个 Reverse ID 启动两次**：第二次会提示注册冲突，不会两个实例互相踢。
- **管理端口起不来**：查看 `data/logs/startup-error.log` 中的具体错误。

## 项目结构

```
phaethon/
├── main.go              # 入口：初始化数据库、加载配置、启动监听、热重载、健康检查、订阅刷新
├── config/              # 配置模型、路由匹配（Rule/Matcher）、限速器
│   └── config.go
├── server/              # 入站协议服务器（Socks5/Trojan/h_tunnel/HTTP/HTTPS/Direct/Reverse）
│   ├── base.go          # BaseServer、AcceptLoop、startTCP/startTLS
│   ├── socks5.go
│   ├── trojan.go
│   ├── htunnel.go
│   ├── http.go
│   ├── direct.go
│   └── reverse.go       # ReverseServer：主动维持反向连接池
├── dialer/              # 出站协议拨号器（代理链逐级拨号）
│   ├── dialer.go        # Dialer/UDPDialer 接口、ChainDial、NewDialer
│   ├── direct.go
│   ├── socks5.go
│   ├── trojan.go
│   ├── htunnel.go
│   ├── hysteria2.go
│   ├── vless.go
│   ├── shadowsocks.go
│   ├── ssh.go
│   └── reverse.go       # ReverseDialer：从 Registry 获取反向连接
├── reverse/             # 反向连接注册中心（被动端使用）
│   └── registry.go      # Registry、ManagedConn、心跳握手（PING/PONG/PENG）
├── tun/                 # TUN 模式（系统级流量拦截）
├── util/                # 日志、Relay、证书生成、连接 ID 等工具
└── data/                # 运行时数据目录（首次运行自动生成）
    └── phaethon.db      # 嵌入式配置数据库（代理/组/规则/映射等均存于此）
```

---

## 配置说明

配置不采用 YAML 文件：所有配置持久化在工作目录下的嵌入式数据库 `data/phaethon.db` 中，
通过 **Web 控制台** 或 **Admin API** 进行增删改查，保存后立即生效。

以下 YAML 片段仅用于说明各配置实体的字段结构（等价于 API/UI 中的数据模型），不是需要手动编辑的文件。

### 配置结构示例

```yaml
proxies:          # 代理节点列表
  - name: NODE1
    type: trojan
    server: 1.2.3.4
    port: 443
    password: xxx
    sni: www.example.com
    skip-cert-verify: true
    udp: false
    proxy: NODE2    # 下级代理（链式代理），可选

subscriptions:    # 订阅源（独立配置，可被多个代理组引用）
  - name: sub1
    url: "https://..."
    interval: 300

proxy-groups:     # 代理组
  - name: Country
    type: best      # select / load-balance / best
    proxies:
      - NODE1
      - NODE2
    subscription: sub1                   # 引用的订阅源名称，可选
    subscription-filter: "HK|TW|JP"      # 按正则过滤订阅节点，可选
    subscription-selected: []            # 选中的订阅节点名称，可选
    health-check-url: http://www.google.com/generate_204
    health-check-interval: 300

rules:            # 路由规则（从上到下匹配）
  - DOMAIN-SUFFIX,example.com,Country
  - IP-CIDR,10.0.0.0/8,DIRECT
  - MATCH,DIRECT

mappings:         # 入站端口映射
  - name: SOCKS5_IN
    type: socks5
    port: 7890
  - name: TROJAN_IN
    type: trojan
    port: 443
    password: xxx
    sni: www.example.com

resolvers:        # 地址重写（类似 hosts）
  - name: R1
    src-host: git.example.com
    src-port: 80
    dst-host: 10.0.0.1
    dst-port: 9000
```

### rules 语法

| 规则类型       | 格式                              | 说明                          |
|---------------|-----------------------------------|------------------------------|
| DOMAIN-SUFFIX | `DOMAIN-SUFFIX,域名后缀,代理名`    | 域名后缀匹配                   |
| IP-CIDR       | `IP-CIDR,CIDR,代理名`              | IP 段匹配                      |
| MATCH         | `MATCH,代理名`                     | 兜底匹配                       |

代理名可以是具体的 proxy、proxy-group，或内置的 `DIRECT`、`REJECT`。

通过 `#MAPPING_NAME` 后缀可限定规则仅对特定 mapping 生效，例如 `LOCAL_HT#TEST_SS5_SRV`。

### mappings 类型

| type         | 说明                          |
|-------------|------------------------------|
| socks5      | SOCKS5 代理入站                |
| trojan      | Trojan 代理入站                |
| h_tunnel    | h_tunnel 协议入站              |
| http        | HTTP 代理入站                  |
| https       | HTTPS 代理入站（自签名证书）    |
| direct      | 直接转发到 dst-host:dst-port   |

上述 mapping 类型都支持附加 `reverse-address` + `reverse-proxy` 等字段，把该入站作为被动反向端使用（见[反向连接](#反向连接reverse)）。

---

## 支持的协议

### 入站（Server）

| 协议      | 文件                  | 说明                     |
|----------|----------------------|-------------------------|
| SOCKS5   | server/socks5.go     | 支持用户名密码认证        |
| Trojan   | server/trojan.go     | 支持 SNI 多路复用         |
| h_tunnel | server/htunnel.go    | HTTP 隧道协议             |
| HTTP     | server/http.go       | HTTP 代理                 |
| HTTPS    | server/http.go       | HTTPS 代理（自签名证书）   |
| Direct   | server/direct.go     | 直接 TCP 转发             |

### 出站（Dialer）

| 协议         | 文件                    | TCP | UDP |
|-------------|------------------------|-----|-----|
| DIRECT      | dialer/direct.go       | ✓   | ✓   |
| SOCKS5      | dialer/socks5.go       | ✓   | ✓   |
| Trojan      | dialer/trojan.go       | ✓   | ✓   |
| h_tunnel    | dialer/htunnel.go      | ✓   | ✓   |
| VLESS       | dialer/vless.go        | ✓   | ✓   |
| Hysteria2   | dialer/hysteria2.go    | ✓   | ✓   |
| Shadowsocks | dialer/shadowsocks.go  | ✓   | ✓   |
| SSH         | dialer/ssh.go          | ✓   | —   | 通过 SSH `direct-tcpip` 转发 TCP；支持密码/私钥/ssh-agent |
| HTTP        | dialer/http.go         | ✓   | —   |

### 代理链

出站拨号自动跟随 `proxy` 字段构建代理链。例如：
```yaml
proxies:
  - name: A
    type: socks5
    server: 10.0.0.1
    port: 1080
    proxy: B
  - name: B
    type: trojan
    server: 10.0.0.2
    port: 443
```
请求走 A → B → 目标。

---

## 编译

### 全平台编译（Makefile）

```bash
make all          # 编译全部 5 个目标平台
make linux        # Linux amd64
make windows      # Windows amd64
make windows7     # Windows 7（需 go-legacy-win7 编译器）
make darwin-amd64 # macOS Intel
make darwin-arm64 # macOS Apple Silicon
```

### Windows 7 兼容性

Windows 7 使用独立编译器 `go-legacy-win7` 编译，避免新版 Go 运行时对 Win7 不兼容的问题。编译前请将 `GO_LEGACY_WIN7` 环境变量指向该编译器，例如：

```powershell
$env:GO_LEGACY_WIN7 = "C:\go-legacy-win7\bin\go.exe"
make windows7
```

---

## 运行

### 环境变量

| 变量       | 说明                                  |
|-----------|--------------------------------------|
| ADMIN_PORT | 覆盖管理面板端口（如 39999）           |
| PHAETHON_LOG_MODULES | 按模块覆盖日志级别（如 `dialer=1,mesh=0`） |
| PHAETHON_WORKER | 标记 worker 进程（内部使用）           |

业务配置（代理、规则、映射等）不从环境变量读取，一律通过控制台或 Admin API 写入数据库。

### 热重载

程序不监视任何配置文件。控制台的修改会立即生效并保存到数据库 `data/phaethon.db`；
如需从数据库重建运行态，请使用 Web UI 中的 **Reload Config** 按钮或重启进程。

---

## 核心模块说明

### config（配置与路由）

- `RuleConfiguration`：配置总入口，包含 Proxies / ProxyGroups / Rules / Mappings / Resolvers
- `RuleConfiguration.Match(*AddrRequest, *Mapping) *Proxy`：按规则顺序匹配目标地址，返回代理
- `RuleConfiguration.Resolving(*AddrRequest) *AddrRequest`：地址重写（resolver）
- `ProxyGroup.Next()`：根据 group 类型（select/load-balance/best）选择节点
- 健康检查：按 `health-check-interval` 定期探测，连续失败 3 次标记死亡，连续成功 2 次恢复

### server（入站）

所有 server 都实现 `ConnHandler` 接口：`HandleConn(net.Conn)`。

新建 server 的推荐模式：
1. 定义 `XxxServer` 嵌入 `BaseServer`
2. 实现 `HandleConn` 处理协议握手
3. 用 `AcceptLoop(listener, handler, name)` 启动监听循环
4. 在 HandleConn 中解析目标地址，调用 `RuleConf.Match()` 选代理
5. 用 `dialer.ChainDial()` 建立出站连接
6. 用 `util.Relay()` 或 `util.RelayWithRateLimit()` 双向转发

### dialer（出站）

所有 dialer 都实现 `Dialer` 接口：`Dial(dstAddr, dstPort) (net.Conn, error)`。

新建 dialer 的推荐模式：
1. 定义 `XxxDialer` 嵌入 `BaseDialer`
2. 实现 `Dial`：先 dial 到代理服务器，再发送协议特定握手
3. 如有 UDP 支持，实现 `UDPDialer` 接口：`DialPacket() (net.PacketConn, error)`

对外统一使用 `dialer.ChainDial(proxy, dstAddr, dstPort)`，它会递归处理代理链。

---

## 反向连接（Reverse）

用于内网穿透场景：被动端（内网）主动向外建立连接，注册到注册表；主动端（公网）从注册表匹配这些连接来传输数据。

### 消息协议

| 消息 | 值 | 发送方   | 说明                              |
|-----|---|---------|-----------------------------------|
| PING| 1 | 客户端   | 心跳包（每 29s 发送）               |
| PONG| 2 | 服务端   | 匹配成功通知，触发 PENG             |
| PENG| 6 | 客户端   | 注册确认 / 握手完成                 |

### 被动端（内网）

配置 `reverse-address` + `reverse-proxy` 的 mapping，启动后持续向 `reverse-proxy` 建立 BIND 连接并注册到 Registry：

```yaml
mappings:
  - name: RV_OUT
    type: socks5
    reverse-address: my-node
    reverse-proxy: OUTBOUND_PROXY
    reverse-max-connections: 3
```

### 主动端（公网）

配置带 `reverse-address` 的 proxy，Match 时会从 Registry 获取已注册的连接：

```yaml
proxies:
  - name: MY_RV
    type: socks5
    reverse-address: my-node
```

例如把某个目标域名/规则指向 `MY_RV`，流量就会通过 `my-node` 的反向连接转发到内网。

### Registry 关键 API

```go
reverse.Refresh()                    // 刷新注册表，关闭旧连接、取消等待者
reverse.GlobalRegistry()             // 获取当前注册表
registry.Register(address, mc)       // 被动端注册连接
registry.Match(address) (net.Conn, error)  // 主动端匹配连接（带 PONG/PENG 握手）
```

---

## TUN 模式

项目包含 `tun/` 目录，实现了基于 TUN 接口 + gvisor 网络栈 + Fake-IP 的系统级流量拦截方案。

TUN 模式需要管理员/root 权限运行，用于拦截系统全部流量并路由到 phaethon。

---

## 多环境配置

每个工作目录拥有独立的配置数据库，多环境请复制完整工作目录，例如：

```
./phaethon-service/
├── phaethon
└── data/
    └── phaethon.db

./phaethon-office/
├── phaethon
└── data/
    └── phaethon.db
```

分别进入对应目录运行 `./phaethon` 即可，各实例的初始化（Admin 端口、认证、node-id）互不影响。

---

## License

Copyright (C) 2026 phaethon authors

This program is free software: you can redistribute it and/or modify
it under the terms of the **GNU Affero General Public License** as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

### 商业授权

AGPL v3 要求：即使仅通过网络提供服务，也必须向用户公开完整源代码（包括修改部分）。

如果你需要在**不开源修改**的前提下将 phaethon 用于商业产品或服务，请通过 [GitHub Issues](https://github.com/whipfeng/phaethon/issues) 联系作者获取**商业许可**。

