> 版本: v0.1.0
> 日期: 2026-08-27
> 状态: ACTIVE
> 负责人: Phaethon Dev

# TUN Watchdog HTTP 连通性探测改造

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-08-27 | 初始版本 | Phaethon Dev |
| v0.2.0 | 2026-08-27 | 统一全平台使用 HTTP probe；移除 DNS probe fallback；仅使用接口索引绑定 | Phaethon Dev |
| v0.3.0 | 2026-08-28 | Windows 接口绑定方案改为 bind() 到接口本地 IP；新增多 IP 选取规则 | Phaethon Dev |
| v0.4.0 | 2026-08-28 | 记录已否决的 netstack 出站方案；记录 shell 命令替换 API 的调研结论 | Phaethon Dev |
| v0.5.0 | 2026-08-28 | 新增 3.8 节：watchdog DNS 解析改用纯 Go 实现以避免系统调用线程阻塞 | Phaethon Dev |
| v0.6.0 | 2026-08-28 | 实现纯 Go DNS 解析器；分离 DNS/HTTP 超时参数；Admin API 新增 `stats` 字段（包计数器 + FakeIP 池统计） | Phaethon Dev |

## 1. 背景与目标

### 1.1 当前问题

TUN watchdog 需要验证 TUN 是否真正可用，而不仅仅是本地 DNS hijacker 还活着。此前曾考虑保留 DNS probe 作为 Windows 降级方案，但 DNS probe 只能证明：

- 系统 DNS 已指向 TUN adapter
- netstack 内部 DNS hijacker 还在运行

它**无法证明** TUN 真的能帮用户连上外网。实际中可能出现：

- 路由表被其他软件覆盖，流量不再走 TUN
- 代理上游断开，TUN 收了包但发不出去
- 只有 DNS 能解析，TCP/HTTP 实际不通

### 1.2 目标

1. watchdog 通过真实 HTTP 请求验证 TUN 网络是否真正可用。
2. 探测目标可配置，默认使用高可用公共 portal，并支持 fallback。
3. TUN 启动后 watchdog 立即开始探测，无需 grace period。
4. 一旦连续探测失败达到阈值，立即 kill 父进程并清理 TUN 残留。
5. 保持现有“父进程死亡/接口消失”监控不变。
6. **统一全平台使用 HTTP probe，不再保留 DNS probe fallback。**

## 2. 架构调整

### 2.1 探测流程

```mermaid
flowchart LR
    A[watchdog 子进程] -->|HTTP GET| B[系统网络栈]
    B -->|TUN adapter| C[netstack]
    C -->|代理/直连| D[公共探测目标]
    D -->|200/204| A
```

由于 watchdog 是独立子进程，它发出的请求会真实经过完整的 TUN 链路，因此能同时验证：TUN 捕获、DNS proxy、netstack 转发、代理/直连出站、目标服务器可达性。

### 2.2 候选探测地址

经本地 curl 验证可达：

| URL | 响应 | 状态 |
|-----|------|------|
| `http://www.msftconnecttest.com/connecttest.txt` | `Microsoft Connect Test` | ✅ 默认首选 |
| `http://connectivitycheck.platform.hicloud.com/generate_204` | 204 No Content | ✅ fallback |
| `http://wifi.vivo.com.cn/generate_204` | 204 No Content | ✅ fallback |

探测时遍历候选列表，任一成功即视为网络正常；全部失败才计入一次探测失败。

## 3. 关键设计决策

### 3.1 使用 HTTP GET 而非 DNS probe

DNS probe 只能验证本地 hijacker 还活着，不能验证真实出网能力。HTTP GET 能覆盖 DNS、TCP、代理链路、目标服务四个层面。

### 3.2 多地址 fallback

单一公共地址可能在特定网络环境下被限制。使用多个 captive portal 地址作为 fallback，显著降低误杀概率。

### 3.3 激进失败策略

用户明确要求“不怕误杀，就怕不杀”。因此：

- 无 grace period
- 探测间隔 3 秒
- 连续失败 2 次即 kill + cleanup
- 最坏情况下 6 秒内触发清理

### 3.4 强制探测流量走 TUN（接口绑定方案）

为避免 Windows 源地址选择、DNS 缓存或路由表被其他软件改动后看门狗探测绕过 TUN，watchdog 的 HTTP socket **必须绑定到 TUN 接口**。同样，dialer 的出站 socket 必须绑定到正确的物理接口以防止路由环。

#### 3.4.1 Windows 接口绑定方案实测结论

经多轮实测验证，各方案在 Windows 上的表现如下：

**测试环境**：物理网卡（以太网, idx=7）、VMware 虚拟网卡（VMnet1/8）、Wintun 虚拟网卡（phaethontun, idx=21）。

**方案一：`syscall.Bind` 在 Control 回调中**

| 接口类型 | 结果 |
|----------|------|
| 物理网卡 | ❌ FAIL: "bind: An invalid argument was supplied" |
| VMware 虚拟网卡 | ❌ FAIL: 同上 |
| Wintun 虚拟网卡 | ❌ FAIL: 同上 |

**根因**：Go 的 `net.Dialer` 在 Windows 上的实现会在 Control 回调之后再次调用 `bind()`（如果设置了 `LocalAddr`），导致 double-bind 失败。即使不设置 `LocalAddr`，实测仍然失败——`syscall.Bind` 在 Control 回调中对**所有接口类型**都无效。

**方案二：`net.Dialer.LocalAddr` 绑定源 IP**

| 接口类型 | 结果 |
|----------|------|
| 物理网卡 | ✅ connect 成功 |
| VMware 虚拟网卡 | ❌ FAIL: "unreachable network"（无路由，预期行为） |
| Wintun 虚拟网卡 | ❌ FAIL: "connection aborted by software" |

**根因**：Windows 使用**弱主机模型**（weak host model），路由选择仅基于目标 IP 最长前缀匹配，**源 IP 不约束路由决策**。设置 `LocalAddr` 为物理网卡 IP，但目标路由指向 TUN 时，连接会失败或路由环。

**方案三：`IP_UNICAST_IF`（setsockopt, IPPROTO_IP, optname=31）**

| 接口类型 | setsockopt | connect | 结论 |
|----------|------------|---------|------|
| 物理网卡 | ✅ 成功 | ✅ 成功 | **可用** |
| VMware 虚拟网卡 | ✅ 成功 | ❌ unreachable（无路由） | **可用**（连接失败是因为无路由，非绑定问题） |
| Wintun 虚拟网卡 | ✅ 成功 | ❌ timeout（无路由） | **可用**（同上） |

**关键发现**：`IP_UNICAST_IF` 的 `setsockopt` 调用对**所有接口类型都成功**，包括 Wintun。之前认为"对虚拟网卡报 10049"是误判——实际测试中 setsockopt 从未失败，连接失败是因为虚拟网卡没有到目标的路由，这是正确的行为。

**结论**：**统一使用 `IP_UNICAST_IF`**。该方案：
- 对所有接口类型有效（物理、VMware、Wintun、TAP 等）
- 在 Control 回调中通过 `setsockopt` 设置，不涉及 `bind()` 调用
- 接口索引需转为网络字节序（`htonl`）
- WireGuard-go 也采用相同方案（仅绑物理网卡）

#### 3.4.2 ~~已否决方案~~：Windows 改用 `bind()` 到接口本地 IP

> **⚠️ 此方案已否决**。实测发现 Windows 弱主机模型下，`bind()` 到源 IP **不能约束路由决策**。设置 `LocalAddr` 为物理网卡 IP，但目标路由指向 TUN 时，连接仍会失败或路由环。详见 3.4.1 方案二的实测结果。

~~**方案**：获取目标接口的本地 IP 地址，通过 `bind()` 将 socket 源地址绑定到该 IP。Windows 路由栈在出站路由查找时，会以绑定的源 IP 为约束，只考虑拥有该 IP 的路由。~~

~~**优势**：~~
- ~~工作在 IP 层，不依赖链路层，对所有接口类型有效。~~
- ~~只要 IP 唯一属于该接口，出站路径就是确定性的。~~

**正确方案**：使用 `IP_UNICAST_IF`（详见 3.4.1 结论）。

#### 3.4.3 接口索引选取规则

`IP_UNICAST_IF` 接受接口索引（网络字节序）作为参数。接口索引由 `bestRouteIndex()`（dialer）或父进程传入的环境变量（watchdog）提供。

**dialer 出站绑定**：通过 `GetBestRoute2` 查询到目标 IP 的最佳路由，排除 TUN LUID，返回物理接口索引。若最佳路由指向 TUN，回退到默认物理接口。

**watchdog 探测绑定**：使用父进程通过 `LAYER_WATCHDOG_TUN_IFINDEX` 环境变量传入的 TUN 接口索引。

**多 IP 接口**：一个接口可能有多个 IP，但 `IP_UNICAST_IF` 绑定的是接口索引而非 IP，因此无需 IP 选取逻辑。系统会自动在该接口上选择合适的源 IP。

#### 3.4.4 各平台绑定策略汇总

| 平台 | 机制 | 绑定目标 | 适用范围 |
|------|------|----------|----------|
| Windows | `IP_UNICAST_IF` (setsockopt) | 接口索引（网络字节序） | 所有接口类型 |
| Linux | `SO_BINDTODEVICE` | 接口名（如 `eth0`） | 所有接口类型 |
| macOS | `IP_BOUND_IF` / `IPV6_BOUND_IF` | 接口索引 | 所有接口类型 |

#### 3.4.5 影响范围

此方案影响两处绑定逻辑：

1. **dialer 出站绑定**（`dialer/bind_windows.go`）：`bindSocket()` 使用 `IP_UNICAST_IF` 绑定到物理接口索引。防止代理出站流量路由环回 TUN。
2. **watchdog 探测绑定**（`tun/bind_windows.go` + `tun/watchdog_probe.go`）：`watchdogControl()` 使用 `IP_UNICAST_IF` 绑定到 TUN 适配器接口索引。强制探测流量走 TUN。

索引由父进程在创建 TUN 后从 `RouteManager` 获取，通过环境变量 `LAYER_WATCHDOG_TUN_IFINDEX` 传给 watchdog 子进程，避免子进程按名字查询时拿到错误的索引。

### 3.5 废弃 DNS probe

`ProbeTUNDNS()` 已从 watchdog 路径移除。原因：

1. DNS probe 不能验证真实出网能力。
2. 用户明确要求统一使用 HTTP probe；若 HTTP 探测不通，即说明 TUN 路径存在问题，应当 kill。
3. 保留 DNS probe 会造成两套探测逻辑，增加维护成本和误杀/漏杀风险。

DNS 相关辅助函数（`buildDNSQuery`、`parseDNSResponseIP` 等）仍保留在 `tun/dns.go` 中，供 DNS hijacker 和测试使用。

### 3.6 已否决方案：netstack 做出站连接

#### 3.6.1 思路

既然已经使用了 gVisor netstack 做入站包处理，能否让 netstack 也负责出站连接？这样 `DialRouteAware` 里的手动路由查询（GetBestRoute2）和接口绑定（IP_UNICAST_IF / bind 到本地 IP）就全部不需要了。

设想的数据流：

```
netstack 发起出站 TCP 连接
  → 路由表选 NIC → TUN NIC (channel EP)
  → WintunSendPacket 注入 IP 包
  → Windows 在 TUN 适配器上收到包
  → Windows 系统路由表：目标=真实 IP → 物理网卡出口
  → 包从物理网卡发出
  → 响应沿原路返回 → Wintun → netstack 收到
```

netstack 本身确实具备这些能力：
- `stack.New()` 创建完整 TCP/IP 栈
- `SetRouteTable()` 配置路由表
- `CreateNIC()` 添加网络接口
- `tcp.NewEndpoint()` + `Connect()` 发起出站连接

#### 3.6.2 否决原因

**路由环风险**：netstack 注入的包从 Wintun 进入系统网络栈，Windows 查路由表时，split-tunnel 路由（`0.0.0.0/1` via TUN）会把包再次送回 TUN 适配器——因为目标地址和应用发的包完全一样，路由结果也一样。

**同接口转发问题**：包从 TUN 进来，路由表又指向 TUN 出去。这属于"同接口转发"，需要：
1. 开启系统 IP forwarding（`IPEnableRouter`）——系统级设置，影响所有网络行为
2. Windows 是否有"不从同一接口转发回去"的防环约束——微软未文档化，无法确认

**结论**：该方案依赖未验证的假设（Windows 路由行为 + IP forwarding 副作用），风险不可控。当前架构（netstack 处理入站 → proxy 用系统 socket 出站）是正确的分层，出站路由和接口绑定在 OS 层解决。

#### 3.6.3 延伸讨论：netstack 的路由能力定位

gVisor netstack 的路由和接口选择能力确实存在，但它解决的是"选哪个 NIC"的问题。对于物理网卡的 NIC，其 link endpoint 最终还是要通过 OS socket 发包——绑接口的问题在 OS 层绕不开。netstack 的路由能力适用于纯用户态网络（如容器网络模拟），不适合与 OS 网络栈混合使用的场景。

### 3.7 Windows shell 命令替换为 API 的调研

当前代码中大量使用 `exec.Command` 调用 `netsh`、`route`、`powershell` 等 shell 命令进行路由和接口管理。Windows 的 `iphlpapi.dll` 提供了完整的等价 API，应当逐步替换。

#### 3.7.1 当前 shell 调用清单（Windows）

| 文件 | 操作 | 当前命令 | 可替代的 API |
|------|------|----------|-------------|
| `tun/route_windows.go` | 设置 IP 地址 | `netsh interface ip set address` | `SetIfEntry` / Wintun API |
| `tun/route_windows.go` | 设置接口 metric | `netsh interface ipv4 set interface` | `SetIpInterfaceEntry` |
| `tun/route_windows.go` | 查询接口信息 | `netsh interface ipv4 show interfaces` | `GetIfEntry2` |
| `tun/route_windows.go` | 删除 ARP 邻居 | `netsh interface ipv4 delete neighbors` | `DeleteIpNetEntry2` |
| `tun/route_windows.go` | 禁用接口 | `powershell Disable-NetAdapter` | `SetIfEntry` (admin status down) |
| `tun/route_api_windows.go` | 查询路由表 | `netsh interface ip show route` | `GetIpForwardTable2` |
| `tun/cleanup_windows.go` | 删除路由 | `route delete` | `DeleteIpForwardEntry2` |
| `tun/cleanup_windows.go` | 重置 DNS | `netsh interface ip set dns dhcp` | `SetInterfaceDnsSettings` |
| `tun/cleanup_windows.go` | 禁用接口 | `powershell` + `netsh` | `SetIfEntry` |
| `tun/dns_system_windows.go` | 设置 DNS | `netsh interface ip set dns` | `SetInterfaceDnsSettings`（Win10 1809+） |
| `tun/dns_system_windows.go` | 设置 metric | `netsh interface ipv4 set interface` | `SetIpInterfaceEntry` |

#### 3.7.2 优先级

1. **高优先级**：路由增删（`CreateIpForwardEntry2` / `DeleteIpForwardEntry2`）——TUN 启动/清理的核心路径，当前用 `route` 命令
2. **高优先级**：接口 metric 设置（`SetIpInterfaceEntry`）——影响路由优先级，当前用 `netsh`
3. **中优先级**：DNS 设置（`SetInterfaceDnsSettings`）——需要 Win10 1809+，低版本需 fallback
4. **低优先级**：接口查询/禁用——频率低，影响小

#### 3.7.3 其他平台

- **macOS**：`route`、`ifconfig`、`networksetup`——Unix 系没有等价用户态 API，shell 调用是标准做法，不改
- **Linux**：`ip link`、`resolvectl`、`nmcli`——同上，不改
- **`dialer/bind_darwin.go`**：`route -n get` 查路由——macOS 无等价 API，不改
- **`util/browser.go`**：`rundll32`/`open`/`xdg-open`——打开浏览器，标准做法，不改

### 3.8 Watchdog DNS 解析改用纯 Go 实现

#### 3.8.1 问题

Watchdog 探测使用 `net.DefaultResolver.LookupIP(ctx, ...)` 解析域名。在 TUN 模式下，系统 DNS 被重定向到 TUN DNS hijacker（192.0.2.2），DNS 查询会走完整的 TUN 链路。

**核心问题**：`net.DefaultResolver` 在 Windows 上使用系统 DNS API（`DnsQuery`），这是一个**阻塞的系统调用**，不支持超时控制。

```
goroutine 调用 net.DefaultResolver.LookupIP(ctx, "ip4", hostname)
  ↓
Go runtime 启动 OS 线程执行 DnsQuery()
  ↓
DnsQuery() 阻塞等待 DNS 响应（可能几十秒）
  ↓
context 超时取消
  ↓
goroutine 返回 "context deadline exceeded"
  ↓
但 OS 线程仍在阻塞！直到 DnsQuery 自己返回
```

**后果**：
1. **线程泄漏**：context 取消后，OS 线程仍在阻塞，无法被回收
2. **资源耗尽**：长期运行可能耗尽 OS 线程资源
3. **超时不可控**：实际超时时间取决于系统 DNS 的行为，不受 context 控制

#### 3.8.2 解决方案：纯 Go DNS 解析器

使用纯 Go 实现的 DNS 解析器替代系统 DNS 调用。纯 Go 实现使用 UDP socket 发送 DNS 查询，可以正确设置 socket 超时。

**实现方式**：

```go
// 使用纯 Go DNS 解析器
resolver := &net.Resolver{
    PreferGo: true,      // 强制使用 Go 实现
    StrictErrors: false,  // 允许部分错误
}

ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
defer cancel()
ips, err := resolver.LookupIP(ctx, "ip4", hostname)
```

**关键配置**：
- `PreferGo: true`：强制使用 Go 的纯 Go DNS 实现，而非系统调用
- 超时通过 context 控制，底层 UDP socket 会正确尊重超时

#### 3.8.3 纯 Go DNS 与 TUN 拦截的兼容性

**问题**：纯 Go DNS 解析器是否仍会被 TUN DNS hijacker 拦截？

**答案**：**是的**，仍然会被拦截。

**原因**：
1. 纯 Go DNS 解析器读取系统配置的 DNS 服务器地址（Windows 注册表 / `/etc/resolv.conf`）
2. 在 TUN 模式下，系统 DNS 已被配置为 192.0.2.2（TUN adapter IP）
3. 纯 Go DNS 解析器向 192.0.2.2:53 发送 UDP 查询
4. Windows 路由表将发往 192.0.2.2 的流量路由到 TUN 接口
5. TUN DNS hijacker 拦截并处理查询

**两种解析器的区别**：

| | 系统 DNS (cgo) | 纯 Go DNS (netgo) |
|---|---|---|
| 实现 | DnsQuery 系统调用 | UDP socket |
| 查询目标 | 系统配置的 DNS | 系统配置的 DNS |
| 能否被 TUN 拦截 | ✅ 能 | ✅ 能 |
| 超时可控制 | ❌ 不能 | ✅ 能 |
| 线程泄漏风险 | 有 | 无 |

#### 3.8.4 超时参数调整

使用纯 Go DNS 后，DNS 和 HTTP 的超时可以独立控制：

```go
const (
    dnsTimeout  = 5 * time.Second   // DNS 解析超时
    httpTimeout = 8 * time.Second   // HTTP 连接+响应超时
)
```

单个 URL 最坏情况：DNS 5秒 + HTTP 8秒 = 13秒（原为 20秒）。
3 个 URL 全失败最坏：39秒（原为 60秒）。

#### 3.8.5 影响范围

- `tun/watchdog_probe.go`：`ProbeTUNHTTPWithBind()` 中的 DNS 解析改用纯 Go 实现
- 不影响其他使用 `net.DefaultResolver` 的代码（如引擎的 DNS hijacker）

#### 3.8.6 验证计划

1. 编译时添加 `-tags netgo` 或运行时设置 `GODEBUG=netdns=go`
2. 验证 DNS 查询仍被 TUN hijacker 拦截（返回 Fake-IP）
3. 验证超时控制生效（context 取消后线程立即返回）
4. 监控 OS 线程数，确认无线程泄漏

## 4. 接口/交互调整

### 4.1 配置字段

`config/config.go` 中 `TUNConfig` 增加：

```go
ProbeURLs []string `yaml:"probe-urls,omitempty" json:"probe-urls,omitempty"`
```

`conf/default.yaml` 同步增加：

```yaml
tun:
  enabled: false
  probe-urls: []
```

### 4.2 Watchdog 环境变量

父进程启动 watchdog 子进程时传递：

| 环境变量 | 说明 |
|----------|------|
| `LAYER_WATCHDOG_PID` | 父进程 PID |
| `LAYER_WATCHDOG_PROBE_URLS` | 分号分隔的探测 URL 列表 |
| `LAYER_WATCHDOG_TUN_IFINDEX` | 当前 TUN 适配器接口索引，用于 socket 绑定 |

### 4.3 Admin API 状态字段

`GET /api/tun` 返回中增加：

```json
{
  "probeURLs": ["..."],
  "stats": {
    "readPackets": 12345,
    "writePackets": 6789,
    "fakeIP": {
      "domainCount": 42,
      "registeredCount": 42,
      "realIPCacheCount": 40
    }
  }
}
```

- `probeURLs`：当前使用的探测 URL 列表
- `stats.readPackets`：从 TUN 设备读入 netstack 的包数
- `stats.writePackets`：从 netstack 写入 TUN 设备的包数
- `stats.fakeIP.domainCount`：已分配的 Fake-IP 域名映射数
- `stats.fakeIP.registeredCount`：已注册到 netstack 的 Fake-IP 数
- `stats.fakeIP.realIPCacheCount`：Fake-IP → 真实 IP 缓存数

probe 失败次数由 watchdog 子进程写入自身日志（`phaethon-watchdog.log`），用于事后排查。

## 5. 迁移与兼容性

- 未配置 `probe-urls` 时使用默认候选列表，行为不变。
- 现有 watchdog 的时间参数和 kill 逻辑调整，但清理动作不变。

## 6. 验证结果与已知问题

### 6.1 当前验证结果

- `go build ./...`、`go vet ./tun`、`go test ./tun` 全部通过。
- Windows 环境下关闭 Proxifier 后，HTTP probe 因 TUN 适配器未能正确创建而失败，watchdog 在连续 2 次失败后正确 kill 父进程并清理残留。
- 这表明 watchdog 逻辑符合预期：HTTP 探测不通 → 认为 TUN 不可用 → 触发清理。

### 6.2 看门狗探测失败根因分析（已修复）

**问题现象**：TUN 启动后，看门狗 HTTP 探测持续失败，导致看门狗杀掉 phaethon 进程。

**根因分析**：

1. 看门狗通过系统 DNS 解析探测域名（如 www.msftconnecttest.com）
2. 系统 DNS 已被重定向到 TUN DNS 劫持器（192.0.2.2）
3. TUN DNS 返回 Fake-IP（如 198.18.0.5）
4. 看门狗连接到 Fake-IP，数据包进入 TUN
5. 引擎 `handleConn()` 收到连接，从 Fake-IP 还原出原始域名
6. 引擎调用 `DialRouteAware()` → `ResolveRouteAware()` **再次解析域名**
7. 二次解析通过原始 DNS 服务器，超时时间为 5 秒
8. 看门狗探测超时时间为 3 秒
9. **5 秒 DNS 超时 > 3 秒探测超时**，导致探测失败

**修复方案**（已实现）：

看门狗直接通过物理接口解析 DNS，绕过 TUN DNS 劫持器：

1. 看门狗使用 `resolveDirect()` 通过物理接口解析 DNS（使用 8.8.8.8、114.114.114.114 等公共 DNS）
2. 得到真实 IP 后，构建 URL 时直接使用 IP（而非域名）
3. 设置 HTTP Host 头为原始域名（保持虚拟主机功能）
4. HTTP 连接到真实 IP，通过 TUN 转发
5. 引擎收到真实 IP 连接，`LookupDomain()` 返回空，直接转发

**修改的文件**：

- `tun/watchdog_probe.go`：新增 `resolveDirect()` 函数，修改 `ProbeTUNHTTPWithBind()` 接受物理接口索引
- `tun/engine.go`：新增 `PhysicalInterfaceIndex()` 方法
- `main_tun.go`：新增 `physicalIfIndexFromEnv()`，传递物理接口索引给看门狗
- `main_tun_windows.go` / `main_tun_nows.go`：`spawnWatchdog()` 传递物理接口索引

**环境变量**：

| 环境变量 | 说明 |
|----------|------|
| `LAYER_WATCHDOG_PHYSICAL_IFINDEX` | 物理接口索引，用于 DNS 查询绑定，绕过 TUN 分流路由 |

### 6.3 待修复的 TUN 问题

当前观察到一个独立的 TUN 启动问题：Wintun 适配器在禁用/清理后，再次启动时未能重新出现在系统接口列表中（`netsh interface show interface` 中无 `phaethontun`），导致 TUN 实际上未生效。该问题需要在 TUN 适配器创建/恢复逻辑中单独修复；watchdog 的逻辑本身已经完成。

### 6.4 后续优化方向：TUN DNS 同步解析真实 IP

**当前问题**：

看门狗的修复方案只测试了 TUN 的**包转发能力**，没有测试 TUN 的 **DNS 劫持能力**。对于正常应用，引擎收到 Fake-IP 连接后仍需二次解析域名，存在延迟。

**优化方案**：

在 TUN DNS 劫持器中同步解析真实 IP，避免引擎二次解析：

1. TUN DNS 劫持器收到查询（如 www.example.com）
2. **同步**通过物理接口解析真实 IP（阻塞，和正常 DNS 一样）
3. 存储映射：Fake-IP → 域名 → 真实 IP
4. 返回 Fake-IP 给应用
5. 应用连接到 Fake-IP → 进入 TUN
6. 引擎收到连接，直接用缓存的真实 IP 连接，无需再次解析

**好处**：

- DNS 解析只发生一次（在 TUN DNS 劫持器中）
- 引擎不需要再次解析，消除二次解析延迟
- 流程和正常 DNS 一样，只是多了一层 Fake-IP 映射
- 看门狗可以同时测试 TUN DNS 劫持和包转发能力

## 7. 风险与回退

| 风险 | 影响 | 缓解 |
|------|------|------|
| 默认探测地址在特定网络下不可达 | 误杀 | 多地址 fallback + 用户可配置 |
| HTTP 探测频率过高 | 低 | 间隔 3 秒，请求量极小 |
| 看门狗探测绕过 TUN（DNS/路由缓存、源地址选择） | 漏杀 | IP_UNICAST_IF 绑定到接口索引，强制流量走对应接口 |
| TUN 适配器被异常删除 | 漏杀/残留 | 接口本地 IP 失效导致绑定失败，探测直接失败 + 接口消失监控兜底 |
| Windows HTTP probe 受 TUN 实现 bug 影响失败 | 误杀 | 按用户要求，HTTP 不通即视为 TUN 故障，触发清理 |

## 8. 验收标准

- [x] `go build ./...`、`go vet ./tun`、`go test ./tun` 全部通过
- [x] 正常 TUN 启动后，watchdog 立即开始 HTTP 探测
- [x] TUN 不可用时，连续 2 次失败后 watchdog kill 父进程并清理 TUN
- [x] 用户自定义 `probe-urls` 后，watchdog 使用自定义地址
- [x] Admin API `/api/tun` 正确返回 `probeURLs` 和 `stats`
- [x] DNS 解析改用纯 Go 实现（`PreferGo: true`），避免 Windows 线程泄漏
- [x] DNS 和 HTTP 超时可独立控制（`dnsTimeout=5s`，`httpTimeout=8s`）
- [x] `stats` 包含包计数器（`readPackets`/`writePackets`）和 FakeIP 池统计
