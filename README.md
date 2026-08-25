# phaethon

L4 代理转发工具（Go 版）。支持多种入站/出站协议、规则路由、代理组、订阅、健康检查、反向连接和 TUN 模式。

## 目录

- [快速开始](#快速开始)
- [零配置使用说明](#零配置使用说明推荐新手)
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

首次运行时，程序会自动把内嵌的 `conf/default.yaml` 复制为工作目录下的 `config.yaml`，因此不需要手动准备配置文件即可启动。

默认配置会开启：
- Web 管理面板：`http://127.0.0.1:9090`
- 本地 SOCKS5 入站代理：`127.0.0.1:7890`

如需自定义代理节点、规则或环境变量，可：
- 在 Web UI 中直接编辑并保存
- 或参考 [`config.yaml.example`](config.yaml.example) 和 [`.env.example`](.env.example) 手动修改
- 敏感信息建议放在 `.env` 中，通过 `${VAR}` 在 `config.yaml` 中引用

---

## 零配置使用说明（推荐新手）

> **phaethon 是什么？**  
> 一个 L4 代理转发工具，把本地服务通过反向通道暴露到公网，也支持正向代理、规则路由、代理组、订阅和 TUN 模式。下面介绍**不做任何 YAML 配置**即可体验的最简用法。

### 1. 启动程序

直接运行编译好的二进制：

```bash
./phaethon
```

启动后 Web 管理面板默认监听 `127.0.0.1:9090`，日志会打印实际地址。如果该端口被占用，启动会失败并在 `.phaethon/startup-error.log` 留下原因。

### 2. 本地 SOCKS5 代理

默认配置已启动一个 SOCKS5 入站代理：

```
socks5://127.0.0.1:7890
```

无需任何出站代理节点即可把流量直接转发到目标（默认规则 `MATCH,DIRECT`）。

### 3. 反向连接（内网穿透）

使用反向连接前，需要先配置一个能连到注册端的出站代理节点。配置完成后，打开反向连接向导：

```
http://127.0.0.1:9090/reverse
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
- **管理端口起不来**：查看 `.phaethon/startup-error.log` 中的具体错误。

## 项目结构

```
phaethon/
├── main.go              # 入口：加载配置、启动监听、热重载、健康检查、订阅刷新
├── config/              # 配置模型、YAML 解析、路由匹配（Rule/Matcher）、限速器
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
└── conf/                # 内嵌默认配置目录
    └── default.yaml     # 默认配置（未提供 config.yaml 时使用）
```

---

## 配置文件

配置采用 YAML 格式，程序从工作目录加载 `config.yaml`。敏感值可以放到同目录的 `.env` 文件中，通过 `${VAR}` 在 `config.yaml` 中引用。

### .env 示例

```bash
PROXY_PASSWORD=your-password
ADMIN_TOKEN=your-token
```

### config.yaml 引用示例

```yaml
proxies:
  - name: NODE1
    type: trojan
    server: 1.2.3.4
    port: 443
    password: ${PROXY_PASSWORD}
    sni: www.example.com

admin:
  enabled: true
  addr: :9090
  token: ${ADMIN_TOKEN}
```

支持的引用语法：`${VAR}`、`$VAR`、`${VAR:-default}`、`$${VAR}`（转义）。

### 配置结构

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
| SSH         | dialer/ssh.go          | ✓   | —   |
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
| ADMIN_PORT | 覆盖管理面板端口（如 9090）            |

`.env` 文件中的变量可在 `config.yaml` 中通过 `${VAR}` 引用。

### 热重载

程序不再使用 `fsnotify` 自动监视配置文件。Admin 面板的修改会立即生效并保存到 `config.yaml`；如果需要从磁盘重新加载配置，请使用 Web UI 中的 **Reload Config** 按钮或重启进程。

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

多环境请复制完整工作目录，例如：

```
./phaethon-service/
├── phaethon
├── config.yaml
└── .env

./phaethon-office/
├── phaethon
├── config.yaml
└── .env
```

分别进入对应目录运行 `./phaethon` 即可。
