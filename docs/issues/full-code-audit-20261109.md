# 全仓功能设计与代码逻辑审计（2026-11-09）

**目的**：为后续讨论与决策提供一份带证据的问题清单，不替代具体修复方案。
**方法**：按模块分组做只读审阅（tun / mesh / p2p / server+handler+reverse / dialer+订阅+健康检查 / db+config+admin），
聚焦正确性、并发、资源生命周期、安全与设计合理性。每条给出 `文件:行号` 证据。
**口径**：

- **已核实**：本轮人工打开代码确认
- **待复核**：审阅时定位到，但未做运行时复现/未逐行确认调用链
- **待验证**：依赖运行环境或特定配置才触发

> 本文不含已修复项。本轮已修复的 watchdog/单实例/TUN 冲突原则见
> `plans/watchdog-residual-kill.md` 与 §七。

---

## 一、执行摘要（Top 10，建议优先决策）

| # | 主题 | 一句话 |
|---|---|---|
| 1 | 管理面认证 | `/api/setup` 公开且无鉴权，可直接改管理员密码；默认还 `AuthEnabled=false` + 绑定 `0.0.0.0`，`/api/db` 能读会话密钥/TLS 私钥 |
| 2 | 数据面缓冲区 | 出站三处 `ToBuffer()` 不 `Release` → 每包泄漏（已修复）；入站"零拷贝损坏"经核实不成立，真零拷贝仍待实现（见 §2.2 C1） |
| 3 | 配置热路径 | `db/matcher.go` 的数据面 `Resolve*` 在锁外读 `m.proxies`，配置变更时可 `fatal error: concurrent map read and map write` |
| 4 | TUN 生命周期 | `StartTUN/StopTUN` 无串行化，热切换可 panic；netstack 的 writeLoop 一次性捕获 `tunCloseCh`，关→开后出向永久不通 |
| 5 | Linux DNS | 启动注册在物理口、停止却恢复 `tun0`，TUN 停止后整机 DNS 指向已消失的 TUN 地址 |
| 6 | P2P 管理面 | 持 `P2PManager.mu` 调用阻塞的 mesh 回调（`UnregisterPeer` 写满即阻塞、`DeleteLinkNIC` 进 netstack）→ 管理面单点冻结 |
| 7 | 入站会话 | 无认证、无会话上限，每连接预留约 12MB 队列 → 连接风暴 OOM；Stripe lane 的 attach 也可被无限放大 |
| 8 | 反向/注册链路 | 注册与控制/数据连接只靠单一口令 + 可猜测字符串，可注册特权端口监听、可劫持 reverse 绑定 |
| 9 | 健康检查 | 启动快照 + 每轮 `PutProxyGroup` 替换组对象（丢 healthMap）+ DIRECT latency=0 → 组选择实际失效或永远选 DIRECT |
| 10 | 规则引擎 | 非法 CIDR 退化为 `0.0.0.0/0`、空 pattern 恒真、未知规则静默丢弃 → 拼错规则即全量放行 |

---

## 二、P0 清单（阻断性/安全）

### 2.1 认证与凭据（admin / 包分发）

| ID | 位置 | 问题 | 后果 | 状态 |
|---|---|---|---|---|
| A1 | `admin/admin.go:995-997`、`:1339`、`:5164-5216` | `/api/setup` 在 `isPublicPath` 中，`apiSetup` 无鉴权直接改写 `AuthEnabled/Username/Password` 并落库 | 未认证即可接管后台，并可下发 reverse-configs | 已核实 |
| A2 | `admin/package.go:432`、`admin/admin.go:1014` | `/api/packages/receive` 免认证且 `io.ReadAll` 无上限 | 未认证远程 OOM/DoS | 待复核 |
| A3 | `db/init.go:146-153`、`main.go:811`、`admin/admin.go:4334-4450` | 默认 `AuthEnabled=false` 且监听 `0.0.0.0:39999`；通用 `/api/db/<bucket>/<key>` 可读写任意 bucket（含 session-secret、TLS 私钥、admin-config） | 默认部署即暴露管理面与凭据 | 待复核 |
| A4 | `admin/admin.go:1045-1065`、`admin/certs/ca.crt` | mTLS 信任仓库内置公共 CA，`VerifyClientCertIfGiven`（证书可选），只比较 `CN`；`GetMeshConfig` 失败时 nodeID 为空，空 CN 可通过 | 持公共 CA 任意客户端证书可登录所有部署 | 待复核 |

### 2.2 数据面正确性

| ID | 位置 | 问题 | 后果 | 状态 |
|---|---|---|---|---|
| C1 | `mesh/link_nic.go:99-104` + `p2p/p2p.go:894`、`:1837-1859` | 原判"零拷贝导致池复用覆盖"**不成立**：本 fork 的 `buffer.MakeWithData` 会拷贝。真正的零拷贝已通过 `MakeWithExternalData`（引用计数归零回调归还 P2P 池）实现 | 无数据损坏，且入站不再每包拷贝 | 已实施（2026-11-09） |
| C2 | `mesh/link_nic.go:167-168`、`mesh/netstack.go:1940-1941`、`mesh/htunnel_endpoint.go:53-54` | `pkt.ToBuffer()` 后只 `Flatten()`，三处都没 `Release()`（正确写法见 `mesh/ipip_tunnel.go:84-85`） | 每包泄漏一个 chunk 引用，内存单调增长 | 已修复（2026-11-09） |
| C2b | `mesh/link_nic.go:151-152`、`:153-195` | 原判"endpoint 必须 DecRef"**有误**：本 fork 的 UDP 发送在 `udp/endpoint.go:470` `defer pkt.DecRef()`，包由调用方拥有；补 DecRef 会 double-decrement（`TestOrdinaryUDPViaLinkNICWrite` 实测 panic）。真正缺的只是 `ToBuffer()` 的 `Release()` | 已修复（2026-11-09） | 已核实（更正） |
| C3 | `mesh/htunnel_endpoint.go:57-70` | 原判"对不属于自己的包 DecRef"同样有误：`DecRef` 是 double-decrement，已删除；缺的是第 53 行 `buf.Release()`。出站改为“复制载荷后释放 ToBuffer 引用”，因为 `peer.Send` 为异步入队 | 已修复（2026-11-09） | 已核实（更正） |
| C4 | `tun/manager.go:258-259` | `AddMeshRoute` 唯一语句是 `return e.AddMeshRoute(subnet)` | 任何调用即栈溢出 | 已核实 |

### 2.3 配置热路径

| ID | 位置 | 问题 | 后果 | 状态 |
|---|---|---|---|---|
| B1 | `db/matcher.go:280`、`:390`、`:508`、`:595`、`:738` vs `:418-458`、`:700-827` | `Resolve*` / `resolveName` 在无锁下读 `m.proxies/m.groups`；`onConfigChange/AddProxy/rewire` 持写锁改同一 map | 配置变更时 `fatal error: concurrent map read and map write`（不可 recover） | 已核实（写入有锁、读取无锁） |
| B2 | `main.go:1937-1970` | 对 `db.GetProxyGroup` 返回的未 `Init()` 组调用 `SetHealthImmediate`，`healthMap==nil` 时写 nil map | 健康检查 API 100% panic | 待复核 |

### 2.4 TUN 生命周期

| ID | 位置 | 问题 | 后果 | 状态 |
|---|---|---|---|---|
| D1 | `tun/manager.go:673-716`、`main.go:106-123` | `StopTUN` 只在开头持锁，后续裸访问字段；`toggleTUN` 无串行化，`Stop` 的 `tunRW.Wait()` 与并发 `Start` 的 `tunWG.Add` 冲突 | 快速开关 TUN 可 panic、泄漏 fd/路由，`e.device=nil` 可覆盖新设备 | 待复核 |
| D2 | `tun/manager.go:496-505`、`mesh/netstack.go:694`、`:1898-1908` | netstack `writeLoop` 启动时一次性捕获 `tunCloseCh`；`StopTUN` 关闭该 channel 后永不重建 | admin 关→开 TUN 后 netstack→TUN 方向永久不通 | 待复核 |
| D3 | `tun/manager.go:610` vs `:688` | 启动时 DNS 注册在物理口（`dnsRegistrationIface`），停止时用 `e.device.Name()`（`tun0`）恢复 | Linux 上停止 TUN 后物理口 DNS 仍指向 TUN，整机解析失效 | 待复核（与历史文档 `graceful_shutdown_and_tun_cleanup_design.md` 同源） |

### 2.5 P2P / server 结构性

| ID | 位置 | 问题 | 后果 | 状态 |
|---|---|---|---|---|
| E1 | `p2p/p2p.go:1642-1652`、`:1219-1233` | 持 `m.mu` 调用 `meshHandler.UnregisterPeer`（阻塞写 `criticalCh`）与 `DeleteLinkNIC`（进 netstack） | P2P 管理面（建链/查询/Stop）单点冻结 | 待复核 |
| E2 | `p2p/stripe_transport.go:501-526` | 先发 primary 子批次，lane 满/超时再把该 lane 批次回退到 `base.SendBatch` | 同一批次内后发帧越过先发帧 → 内层 TCP 乱序/收缩窗口，与 lane 亲和设计矛盾 | 待复核 |
| E3 | `p2p/p2p.go:1113-1169`、`:536-543` | 入站会话零认证、无会话数上限；每 peer 预分配 256 队列 × 512 深，约 4MB 通道环 + 8MB 预算 | 连接风暴 OOM；`StripeAttach` 仅凭 `(NodeID, RemoteSeq)`，lane 数无上限 | 待复核 |
| F1 | `server/base.go:118-140`、`server/socks5.go:50` | 入站监听硬编码 `:port`（0.0.0.0）；socks5 在密码为空时走 no-auth | 变成开放代理，且无法只绑回环 | 待复核 |
| F2 | `server/control_server.go:290-469` | `register` 无独立鉴权，`PreferredPort` 只校验 1-65535 | 可占据注册机特权端口并对外监听 | 待复核 |

---

## 三、分模块发现（P1 / P2）

### 3.1 tun

| 严重度 | 位置 | 问题 |
|---|---|---|
| P1 | `tun/manager.go:519-590` | `StartTUN` 全程持 `e.mu` 做提权、清理（Windows 最长 15s 轮询）、创建设备、等就绪（最长 10s）；期间所有 getter 阻塞，admin 状态接口卡死；`CleanupResidual` 被调用两次（`main_engine.go:67` 与 `manager.go:538`） |
| P1 | `tun/route.go:161-175`、`route_linux.go:157-166`、`route_windows.go:139-163` | `platformSetup` 中途失败无回滚（Linux 已改 sysctl/可能已插 iptables；Windows 已加路由/改 weak-host/设 IP），且 `applied=false` 使后续 `Teardown` 直接短路 → 系统停在半接管态 |
| P1 | `tun/route_linux.go:245-255` | 非 bypass 模式下 `originalRpFilter/originalIPForward` 恒为 0，停止时会把 `ip_forward` 写成 0 |
| P1 | `tun/manager.go:610-612` | `setSystemDNS` 失败只 warn，路由已全局接管但 DNS 未重定向 → 静默泄漏/域名规则失效，API 仍显示 running |
| P1 | `tun/cleanup_windows.go:109-123` | 每次启动清理都把默认网关物理口 DNS 清成 DHCP，用户静态 DNS 每次被抹掉且不恢复 |
| P1 | `tun/cleanup_darwin.go:18-54`、`dns_system_darwin.go:43-45` | Darwin DNS 备份只在内存，SIGKILL 后整机 DNS 永久指向已消失的 utun |
| P1 | `tun/route_windows.go:295-299` | `getInterfaceLUID` 失败即 return，跳过物理口 weak-host 恢复 |
| P1 | `tun/manager.go:955`、`:963` | `pktBuf` 注入后用归还池（注释假设 gVisor 已拷贝）；若注入路径异步/克隆则内容被覆写 |
| P1 | `tun/manager.go:783`、`:705-708` | `readLoop` 裸读 `e.device`，Stop 在锁外 `Close()` 并置 nil；Linux `close(fd)` 不一定唤醒阻塞中的 `read(2)` |
| P2 | `tun/manager.go:687-708` | Stop 在锁外写 `device/routeMgr/dhcpSrv`，getter 在锁内读同一批字段（data race） |
| P2 | `tun/route_api_windows.go:718-786` | 全表删除 `0.0.0.0/0 + next-hop 0` 的默认路由，不按 LUID 过滤；`rowSize=104` 硬编码 |
| P2 | `tun/route_darwin.go:141-185` | `preemptForeignSplits` 删除其他 VPN 的 0/1，快照仅存内存，崩溃后永久丢失 |
| P2 | `tun/cleanup_linux.go:80`、`:92-110` | 恢复 `rp_filter("all")=2` 而非原值；iptables 删除硬编码 `tun0` |
| P2 | `tun/manager.go:458-464`、`main.go:180-182` | `Stats().FakeIP` 恒零；`deviceName` 硬编码 `PhaethonTUN` |
| P2 | `tun/admin_windows.go:41-94` | 运行期热开 TUN 遇非管理员会 UAC 重启进程（`os.Exit(0)` 跳过 closeAll） |
| P2 | `tun/route.go:220` | `SplitTunnels` 与 `applied` 无关，未接管时 UI 仍显示接管中 |

### 3.2 mesh

| 严重度 | 位置 | 问题 |
|---|---|---|
| P1 | `mesh/netstack.go:414`、`mesh/mesh.go:631-641` | `SelectRoute` 每包 `GetRouteTable()` 做整表 `make+copy`（已建好的 trie 未用） |
| P1 | `mesh/mesh.go:176-235`、`:974-1013` | 注释称"static 优先"，实现排序后 `hash % len(available)`，static 优先级被完全忽略 |
| P1 | `mesh/mesh.go:1019-1033`、`:902-904`、`:1134-1140` | `RegisterPeer/UnregisterPeer/HandleTopologyGossip` 裸 `criticalCh <-`，不 select closeCh；`Stop` 无 Once/wg → 停止后回调永久阻塞 |
| P1 | `mesh/topology.go:280-299` | `GetAllPeers` 只浅拷贝指针，criticalWorker 在锁内原地改 `PeerInfo`，gossip/读侧在锁外读 → data race |
| P1 | `mesh/netstack.go:1736`、`:1811`、`:1847-1849` | 每个 accepted TCP / UDP 流各起一个不受 wg 管理的 goroutine，无全局上限；`r.Complete(false)` 让握手期上限失效 |
| P1 | `p2p/p2p.go:1833-1859` | 按 `FlowKey%4` 钉队列，队列满在会话读循环里等 100ms → 背压反向传播，pong 也被延迟 |
| P1 | `p2p/p2p.go:536-543`、`frame/frame.go:48-52` | 入站队列 4×4096 + DNS 4096，每槽 64KB 池化缓冲 → 最坏约 1.3GB，且无字节预算 |
| P1 | `mesh/route_selector.go:158-164`、`mesh/netstack.go:442-495` | 选路失败一律落 `LocalStack=true, EgressNIC=1`，mesh 目的地被吸回本地栈后静默丢弃（无 ICMP/无计数） |
| P1 | `mesh/fakeip.go:113-162` | `Lookup` 在全局写锁内做同步 DB 写并回调 `onChange`；池满时分配循环可能不退出且持锁 |
| P2 | `mesh/dns.go:575-580` | `dnsQueryCh` 满时静默丢弃，不回 SERVFAIL |
| P2 | `mesh/dns.go:594-660` | 非 A 查询也返回 A 记录（qtype 与 answer 不符） |
| P2 | `mesh/dns.go:279-296` | 转发响应只按域名匹配 pending，不校验来源 IP/TXID |
| P2 | `mesh/link_nic.go:185-194` | 发送失败仅 `continue`，返回成功；gVisor 只能等重传 |
| P2 | `mesh/mesh.go:231`、`:303`、`:1010` | `stickyCache` 与 `PeerQualityTracker.peers` 从不清理，长期 churn 下无界增长 |
| P2 | `mesh/mesh.go:730-732`、`:2089-2092` | `onRouteChange` 无锁读写 |
| P2 | `mesh/mesh.go:3433-3441` | Dijkstra 等 cost 时选路结果随机（map 遍历） |
| P2 | `mesh/fakeip.go:255-257` | `ip.To4()` 为 nil 时 panic（DB 脏数据可触发启动崩溃） |
| P2 | `main_engine.go:234-239` | `meshMgr.Start()` 早于 `ns.SetMeshManager()`，启动窗口内入站 mesh 包无路由被丢 |

### 3.3 p2p

| 严重度 | 位置 | 问题 |
|---|---|---|
| P1 | `p2p/stripe_transport.go:438-461` | `laneEnqueue` 在写路径同步等 50ms，慢 lane 阻塞整条会话写循环（gossip/pong/DNS 一起延迟） |
| P1 | `p2p/p2p.go:1246-1258`、`stripe_transport.go:206-221` | `attachStripeLeg` 只按 `(NodeID, RemoteSeq)` 匹配，无认证；lane 数无上限 |
| P1 | `p2p/p2p.go:1898`、`:1940` | 协议版本严格相等，无能力协商 → 滚动升级期间全量 flapping |
| P1 | `p2p/p2p.go:782-794` | 注释称 control 优先，实际 `select` 在同时就绪时随机 |
| P1 | `p2p/lane_flow.go:465-487` | `writeCredit` 把所有 gated lane 的余量求和，per-lane cap 被架空 |
| P1 | `p2p/p2p.go:326-347`、`:253-257` | `dataBudget` 在 DRR 出队即退款，`drrOutput`（512 帧）未计入预算 |
| P1 | `p2p/udp_leg_transport.go:268-279`、`:296-324` | UDP 腿只校验源端口，`epPump` 连源地址都不校验，无 MAC |
| P2 | `p2p/p2p.go:1745`、`:2315`、`:1579` | `transport/leg` 字段读写锁保护不一致（race） |
| P2 | `p2p/p2p.go:1290-1302`、`:1663-1707` | `StopPeerByProxyName` 与在途重连竞争，已停止的 proxy 可能复活 |
| P2 | `p2p/p2p.go:539-549`、`:581-597` | 6 个 `meshInboundLoop` worker 未纳入 wg，Stop 顺序相反 |
| P2 | `p2p/cache.go:197-223`、`:45-52` | `StoreFromReader` 无大小上限/校验和，临时文件名固定；cache key 白名单过宽 |
| P2 | `p2p/p2p.go:2812-2816` | 两个无 tag 版本用字符串序比较，可能得出相反结论 |
| P2 | `p2p/p2p.go:1039-1056` | 非 IPv4/截断载荷 hash 恒 0，与所有异常帧挤 `dataQueues[0]` |
| P2 | `p2p/p2p.go:1865-1868` | `FrameLaneAck` 空实现，无 stripe 的会话下静默丢弃；unknown 帧每帧 INFO 日志 |

### 3.4 server / handler / reverse

| 严重度 | 位置 | 问题 |
|---|---|---|
| P1 | `reverse/registry.go:416-428`、`:166-197` | 数据/控制连接配对只靠明文 `address`，无 nonce/凭证 → 可注册伪造 bottom 连接分享/劫持流量 |
| P1 | `server/trojan.go:34-51`、`:546` | Trojan 握手无读超时，`io.ReadFull` 与 TLS `Handshake` 可被静默连接永久占住 |
| P1 | `server/ssh.go:96-128` | `NewServerConn` 无握手超时；session channel 无限并发；公钥回调不校验用户名/来源 |
| P1 | `handler/connection_handler.go:197-238` | 内建 SSH/console 在 handler 为 nil 时未 return，回退直拨 localhost → 暴露宿主 sshd |
| P1 | `server/control_server.go:188-205`、`:227-231` | 心跳 goroutine 与 `runSession` 并发 `WriteFrame` 同一 conn，写非原子 → 控制帧错位 |
| P1 | `server/control_server.go:392-457` | 同一控制连接重复 `register` 会新建 listener/proxy/matcher，旧的只能清一个 |
| P1 | `reverse/registry.go:194-197`、`:272-275` | bottom 连接池与 waiter 队列无上限；客户端心跳使连接永不过期 |
| P1 | `server/htunnel.go:986-996`、`:705-799` | 通道 ID 为自增 int64 的 `X-I`，GET/POST/DELETE 不校验凭据 → 可枚举杀通道/注入 |
| P1 | `server/trojan.go:49` | 认证失败日志打印映射口令的 SHA224 前值 → 日志泄露即可离线冒用 |
| P2 | `server/trojan.go:400-407` | 成功读 UDP 回包后 `seenTargets.Put` 为真即 `IsClosedErr(nil)` 并 return → 首个回包就终止读循环 |
| P2 | `server/htunnel.go:829`、`:1015-1017` | `io.ReadAll` 无上限，http.Server 无超时 |
| P2 | `server/control_server.go:477-561` | 分配端口持全局锁并逐端口 `db.ListMappings()` + `net.Listen` 探测 → 管理面数秒级阻塞 |
| P2 | `server/control_server.go:127-138` | `SetPortRange` 只改内存不写 DB，重启回退 |
| P2 | `server/socks5.go:423-437` | UDP relay 的 `clientAddr` 被任意来包覆盖，不校验与控制连接对端一致 |
| P2 | `server/trojan.go:106-108` | 不校验结尾 CRLF；口令用 `!=` 非恒时比较 |
| P2 | `handler/connection_handler.go:284-293` | `localNodeDomain` 第二分支不可达（死代码） |
| P2 | `handler/connection_handler.go:337-345`、`:484-491` | 直连只取 IPv4；UDP 在 IPv6-only 时 `dialIP=nil` 仍调用 |
| P2 | `server/reverse.go:72-99`、`:242` | 已匹配交付的连接脱离 `connections`，`Close` 无法强制收敛 |
| P2 | `server/listener.go:12-27` | 入站支持类型与 dialer/配置不对称（vless/shadowsocks/hysteria2 无入站） |

### 3.5 dialer / 订阅 / 健康检查

| 严重度 | 位置 | 问题 |
|---|---|---|
| P1 | `main.go:2344-2404`、`:2502` | 健康检查用启动快照，每轮 `db.PutProxyGroup(g)` 覆盖用户/订阅刚写入的配置，已删除的组被"复活" |
| P1 | `db/matcher.go:726-748`、`config/config.go:300` | `PutProxyGroup` 替换 matcher 组对象但新副本 `healthMap==nil` 且不 `Init()` → 健康数据永远不生效，best/load-balance 退化 |
| P1 | `main.go:2469-2496`、`:2524-2561` | 槽等待固定 5s 而单次预算可 35s → 第 9 个成员起未探测即判死；`wg.Wait()` 无总超时，一个挂死 dialer 让整个周期停摆 |
| P1 | `dialer/dialer.go:123-128` + 各实现 | `ContextDialer` 只被 Direct/Socks5 实现；链内下一跳一律无 ctx；SSH channel open、HTTP 无任何 deadline |
| P1 | `config/config.go:768-771`、`main.go:2461-2463` | DIRECT 健康记录 latency=0 → 含 DIRECT 的 best 组永远选 DIRECT |
| P1 | `dialer/ssh.go:164-175`、`:111-134` | SSH client 池按 `lastUsed` 判空闲，会掐断仍在转发的长连接；握手在全局 `sshCacheMu` 内完成，所有 SSH 拨号串行化 |
| P1 | `dialer/htunnel.go:284-356`、`:484-486` | `Read` 遇 408/空 body 无限重试，`SetDeadline` 是 no-op → 健康检查 deadline 无效，goroutine 永久自旋 |
| P1 | `dialer/bind_strategy.go:98-112` | BIND/reverse/P2P 用 `db.GetProxy(ViaProxy)`（DB 副本，Next 恒 nil），找不到时静默直连 → 流量泄漏 |
| P1 | `dialer/http.go:21-63` | HTTPDialer 忽略 `Next`，TCP 与 `ReadResponse` 无 deadline |
| P1 | `main.go:2759-2856` | 订阅刷新无最小节点数/比例阈值，空 body 会清空节点池；DB 回退分支不重建组；`ActiveMember` 写绕过锁 |
| P2 | `dialer/hysteria2.go:39-90` | 池键只用 `proxy.Name`，配置变更不失效；`getClient` 持锁建连；`SetDeadline` no-op |
| P2 | `dialer/htunnel.go:118-128` | `sharedHTunnelClient` 重建前不 `CloseIdleConnections`，且用指针做签名 → 每次 reload 都重建 |
| P2 | `config/subscription.go:222-233` | 节点池以 `Name` 为唯一键，同名去重、空名退化为 server:port；流量提示节点也会参与健康检查 |
| P2 | `config/config.go:885-910` | `RemoveHealth` 无调用者；`CopyHealthFrom` 只复制指针，两个组共享可变结构 |
| P2 | `config/config.go:39-41`、`db/configstore.go:151`、`dialer/ssh.go:196` | 凭据以明文 JSON 存 Bolt；SSH 始终 `InsecureIgnoreHostKey()` |
| P2 | `dialer/dialer.go:22-32`、`:60-71` | UDP 端口分配器全局变量无同步 |
| P2 | `dialer/dialer.go:448`、`socks5.go:217`、`htunnel.go:213` | 日志占位符与参数数量不匹配（`%!d(...)`） |
| P2 | `dialer/reverse.go:381-386` | `SetDeadline` 改共享 socket 的 deadline，打断内部心跳/读环 |
| P2 | `dialer/dialer.go:282-285`、`:345` | `meshAwareDialer` 不转发 `MaxDialTimeout()`；hysteria2 用 TCP 探活（QUIC 只开 UDP）必然判死 |

### 3.6 db / config / admin

| 严重度 | 位置 | 问题 |
|---|---|---|
| P1 | `admin/admin.go:1045-1065` | 见 A4：mTLS 实际非强制 |
| P1 | `config/config.go:1143-1155` | 非法 CIDR/IP 回退 `0.0.0.0/0` → 规则拼错即全量放行 |
| P1 | `db/matcher.go:146-167` | `parseRule` 段数<3 时 `Pattern=""`（`HasSuffix` 恒真）；`no-resolve` 被当代理名；未知类型静默丢弃 |
| P1 | `config/config.go:1124-1131` | 域名后缀大小写敏感且无标签边界 → `notexample.com` 命中 `example.com` |
| P1 | `admin/admin.go:2032` | `/api/config/admin` 直接序列化含密码/token/私钥的配置，无脱敏 |
| P1 | `admin/admin.go:4334-4450` | 通用 `/api/db` 可读写任意 bucket（见 A3） |
| P1 | `admin/package.go:509`、`:468` | 接收包强制 `Published=true` 并触发版本检查（可 `os.Exit`）→ 重放已签名包即可触发升级/重启 |
| P1 | `admin/package.go:112`、`:125` | `ParseMultipartForm` 不是总量限制，`header.Size` 落盘后才检查 |
| P1 | `admin/admin.go:774-780` | 自动证书失败仅 warn 后回退明文 HTTP；只配 cert 不配 key 也静默走自动证书 |
| P2 | `admin/admin.go:5034-5041`、`:970-992` | 会话 Cookie 无 `Secure`、固定 7 天、无吊销；改密后旧会话仍有效 |
| P2 | `admin/admin.go:92-106` | 无条件信任 `X-Forwarded-For`；登录失败表/验证码表无上限 |
| P2 | `admin/admin.go:1281-1289` | pprof/stall-dump 与业务同 mux，默认无认证时可公网访问 |
| P2 | `db/configstore.go:602` | `make([]uint64,0,len(order)-1)` 在 order 为空时 panic |
| P2 | `db/configstore.go:361-370`、`:509-513` | `ruleKey` 直接 append 全局前缀；`next_id` 缺失时从 1 开始可覆盖既有规则 |
| P2 | `db/version_notify.go:44-57`、`:10-15` | 每次 Put 同步开写事务并 fsync；`BucketBindings` 未加入 skip，端口绑定被当配置变更 |
| P2 | `db/configstore.go:332-340` | `PutReverseRuntime` 跨两个事务读改写，并发互相覆盖 |
| P2 | `db/matcher.go:477-503` | 只实现 DOMAIN-SUFFIX/IP-CIDR/MATCH，`DOMAIN` 规则被静默忽略 |
| P2 | `admin/admin.go:4500-4541`、`:1828-1871` | mesh 读改写吞错误，`GetMeshConfig` 失败会 `PutMeshConfig(nil)`；多处先热更新后落库 |
| P2 | `admin/templates/setup.html:417` | 期望 `data.proxies`，接口返回数组 → setup 向导代理列表失效 |

---

## 四、需要决策的设计问题（建议逐条拍板）

1. **管理面默认安全模型**：默认是否强制回环 + 首次启动必须 setup token？`/api/db` 是否下线？
2. **mTLS 定位**：是否改为每部署独立 CA + `RequireAndVerifyClientCert`？内置公共 CA 是否废弃？
3. **入站 P2P 认证**：是否引入节点级密钥/会话 nonce？会话数、lane 数、队列是否改为惰性分配？
4. **包分发的信任链**：接收端是否要求认证？收到新包是否只"暂存"，升级需管理员确认？
5. **监听地址**：是否给 mapping 增加 `listen_addr`（默认回环）？这会改变现有部署行为。
6. **reverse 绑定身份**：是否用 HMAC(nonce, 节点密钥) 替代明文 address？
7. **TUN 生命周期**：是否引入显式状态机（Idle/Starting/Running/Stopping）并串行化所有 admin 操作？
8. **健康检查语义**：DIRECT/REJECT 的排序、预算与槽等待、组对象热替换时如何保留 health 状态？
9. **ctx 贯穿范围**：是否把所有 dialer 与链内跳都改为 ctx 版本（工作量较大但一次性收敛）？
10. **规则严格化**：非法规则/CIDR 是否拒绝保存（而不是退化为全量）？`DOMAIN` 规则是否补齐？

---

## 五、建议修复批次

| 批次 | 内容 | 验收 |
|---|---|---|
| 1 安全止血 | A1（setup 鉴权）、A2/A3（包上传与 `/api/db`）、F1（监听地址 + 空密码拒绝）、F2（注册鉴权/端口策略） | 未认证请求无法改配置/上传/读密钥；默认只监听回环 |
| 2 数据面正确性 | C1/C2/C3（包生命周期）、C4（自递归）、B1（matcher 快照或 RLock） | `go test -race`；大流量压测无内存增长、无数据损坏 |
| 3 TUN 生命周期 | D1/D2/D3 + 启动失败回滚 + DNS 写读对称 + Windows 适配器清理 | 反复 admin 开关 TUN；`kill -9` 后重启能恢复 DNS/路由 |
| 4 P2P 背压与控制面 | E1（锁外回调）、E2（批次排序）、E3（上限+认证）、lane/预算/credit 修正 | peers churn 压测无 flapping；无管理面冻结 |
| 5 健康检查与 ctx | 快照刷新、组对象保留 health、DIRECT 排序、ctx 贯穿、deadline 实现 | 断网/慢节点下组选择正确；健康检查无 goroutine 泄漏 |
| 6 规则与配置一致性 | 规则严格校验、DOMAIN、迁移幂等、先落库后应用 | 规则误配不放大为全量放行；重启后 DB 与运行态一致 |

---

## 六、待验证清单（不要直接当缺陷修）

- 零拷贝优化：是否为本 fork 增加"外部内存 chunk + release 回调"，以省掉入站每包一次拷贝（性能项，非正确性）
- 3.3 `stripe_transport` 回退 primary 的实际乱序影响（需要抓包/内层 TCP 统计）
- E1：`UnregisterPeer` 阻塞写是否真会在正常负载下打满 `criticalCh`
- B2：`SetHealthImmediate` 的 nil map panic 是否被上层 recover
- 3.1 `read(2)` 与 `close(fd)` 的唤醒行为（Linux/Darwin 差异）
- 3.5 订阅源中同名节点/提示节点的实际比例
- 3.2 `hysteresis` 是否会永久钉死 100% 丢包的 incumbent

---

## 七、本轮已修复（2026-11-09）

- 管理面：`/setup` 与 `/api/setup` 从公开路径移除，改为遵守 `AuthEnabled` 规则（未开认证可用，开了认证必须先登录）——`admin/admin.go` `isPublicPath`
- watchdog 职责收窄：删除 `/proc` 进程清扫，改为 `data/instance.lock` 单实例锁（flock / LockFileEx）
- 停机判定改用内存 `stopping`，删除 `data/state/stopped` marker
- 新增 `--takeover`（Linux/macOS）：校验锁持有者后 SIGTERM 旧 watchdog/worker 并等锁
- Windows 重启用 `os.Kill`（SIGTERM 不被支持）；watchdog `LockOSThread`；worker Pdeathsig 增加 `getppid` 竞态检查
- 详见 `plans/watchdog-residual-kill.md`

---

## 八、决策记录（2026-11-09，逐条过审用）

> 规则：每条先定结论，再统一实施；实施状态单独标注。

### D1 管理面认证（`/api/setup`）

- **结论**：按 `AuthEnabled` 规则统一。未开认证 → 可用；已开认证 → 必须先通过 mTLS/session/签名/token 认证。
- **不做**：不引入 bootstrap token，不要求 CLI 初始化。
- **状态**：已实施（`admin/admin.go` `isPublicPath` 删掉 `/setup`、`/api/setup`）。

### D2 默认认证与首次启动

- **结论**：非交互式启动维持现状——默认不开认证、监听 `0.0.0.0`，日志保留 Warn 提示。
- **交互式启动要完善 4 点**（待 D 系列全部定完后统一实施）：
  1. 终端判断改用 `term.IsTerminal`，避免 `/dev/null` 被误判为终端
  2. 询问监听地址，默认 `127.0.0.1`，可选 `0.0.0.0`；端口探测使用所选地址
  3. 选择"不认证"时打印明确风险提示
  4. 密码最小长度校验；Mesh node-id 格式校验
- **状态**：待实施。

### D3 包分发与升级：只提醒，用户决定

- **结论**：不再自动发布、自动升级、自动转发。
- **接收/下载**：只落到 pending 目录，不进入运行候选集；管理界面提示"发现新版本"，并列出可下载版本。
- **升级**：用户选定版本后标记"已启用"（pin 文件记录目标版本），提供"立即重启"与"下次重启生效"两个选项。
- **watchdog**：只认已启用的版本；pending 不参与选择；找不到时回退 last-good；允许降级到任意已下载版本。
- **分发**：默认不自动转发，改为管理员手动触发分发（避免未认证请求放大）。
- **安全**：请求体大小上限（无条件）；receive 鉴权因不再自动升级而降级为 P2（防磁盘/带宽滥用）。
- **状态**：已实施（2026-11-09）。

### D4 通用数据库接口 `/api/db/**`

- **结论**：保留，但默认关闭；启动时显式开启（`--enable-debug-api`，兼容环境变量 `PHAETHON_DEBUG_API=1`）。
- **只读**：仅允许 GET；POST/DELETE 一律 403（写入走业务 API 或离线工具）。
- **白名单 bucket**：`proxies`、`proxy-groups`、`subscriptions`、`subscription-nodes`、`rules`、`mappings`、`resolvers`、`reverse-configs`、`bindings`、`port-range-config`、`tun-config`、`mesh-config`、`versions`、`logs`、`webhooks`。
- **禁止访问**：`admin-config`、`runtime-state`、`certificates`、`ssh`、`ssh_keys`、`api-keys`、`audit-log`。
- **限制**：单次列表返回条数上限；请求体大小上限（防御性）。
- **调试端点**：pprof、stall-dump 等与 `/api/db` 共用同一个调试开关，默认关闭。
- **状态**：已实施（2026-11-09）。

### D5 mTLS 客户端证书

- **结论**：维持现状，不改架构。继续使用内置共享 CA + 可选客户端证书 + CN 校验（`all` 或当前 nodeID）。
- **否决**：每部署独立 CA 方案（会破坏多节点 mesh 互信，且客户端证书分发成本高）。
- **保留的已知风险**（记录在案，不再讨论）：
  - `all` 证书在所有部署通用，单点泄露影响面大
  - 内置 CA 为项目级共享，私钥泄露影响所有部署
  - `VerifyClientCertIfGiven` 表示 mTLS 为可选验证，强认证仍依赖密码/token
- **状态**：维持现状。

### D6 数据面包内存管理

- **结论**：出站泄漏已修复；入站真零拷贝遗留到下一轮。
- **出站（已实施 2026-11-09）**：
  - `netstack.go` 的 `handleWriteLoopPacket` 增加 `defer buf.Release()`。
  - `link_nic.go` / `htunnel_endpoint.go` 的 `Send` 是异步入队，改为“复制载荷 + `buf.Release()`”，端点不再 `DecRef`。
  - **实测更正**：本 fork 的 UDP 发送在 `udp/endpoint.go:470` `defer pkt.DecRef()`，包由调用方拥有；原审计建议的“endpoint 必须 DecRef”会导致 double-decrement（测试 panic），已推翻。
- **入站（已实施 2026-11-09）**：
  - `gvisor-fork/pkg/buffer` 新增 `MakeWithExternalData(data, release)`：外部内存 chunk，引用计数归零时回调；COW/Clone 复制到池化 chunk。
  - `LinkNIC.InjectInboundOwned` / `MeshManager.HandleMeshFrameOwned`：所有丢弃路径调用 release，正常路径交给 gVisor 引用计数。
  - `meshInboundLoop` 通过可选 `ownedMeshHandler` 接口走零拷贝，旧 handler 自动回退拷贝路径。
- **状态**：全部已实施（2026-11-09）。

### D7 配置热路径并发（matcher）

- **结论**：改为**不可变快照 + 原子替换**。写路径构建新的代理/规则/解析器快照后整体替换；数据面 `Resolve*` 无锁读快照。
- **顺带修复**：健康检查组对象被 `PutProxyGroup` 替换后丢失 `healthMap` 的问题，改用同一套快照机制保留运行态。
- **否决**：读路径加 RLock（热路径开销）、sync.Map（仍有 TOCTOU 与一致性问题）。
- **状态**：已实施（2026-11-09）。

### D8 TUN 启停生命周期

- **结论**：引入显式状态机，替换“只有一个布尔标志”的现状。
- **状态**：`Idle / Starting / Running / Stopping`，非法转换直接拒绝并返回错误。
- **串行化**：所有启停与热切换（admin 开关、启动失败回滚）走同一控制路径，不允许并发交叉。
- **写入循环**：不再一次性捕获 TUN 的关闭信号；改为按运行状态判断，或订阅一个只在 StopStack 关闭的稳定信号。
- **DNS**：启动时记录实际注册的接口名（Linux 是物理口），停止时用同一个名字恢复。
- **失败回滚**：`StartTUN` 任一步失败，按已执行步骤逆序撤销（路由、sysctl、iptables、IP、weak-host、DNS）。
- **状态**：已实施（2026-11-09）。

### D9 P2P 清理与全局锁

- **结论**：清理动作串行化到专用后台协程，绝不在 P2P 全局锁内调用 mesh/netstack。
- **做法**：删除 peer 时锁内只摘表并投递清理请求；清理协程串行执行“通知 mesh 下线、删网卡、停会话”。
- **否决**：把 mesh 通道改成非阻塞投递（丢事件会让拓扑表残留幽灵节点，排查困难）。
- **状态**：已实施（2026-11-09）。

### D10 多链路发送乱序

- **结论**：按流整组路由，不做“部分帧回退主链路”。
- **做法**：同一条内层流的帧统一选一条路径（旁路或主链路）；该旁路不可用/已满时，整条流改走主链路。
- **目的**：保证流内有序；旁路在可用时继续承担流量。
- **状态**：已实施（2026-11-09，改为非阻塞整组回退并固定到主链路）。

### D11 P2P 入站认证与资源上限

- **认证**：不做会话级认证。能建立 P2P 连接的前提是已通过底层代理链路认证，不重复造轮子。
- **唯一配置项**：最大入站会话数，默认 64，范围 1–1024，重启生效（加入 mesh/P2P 配置）。
- **固定值**：每会话附加链路（lane）上限 8；队列深度保持 512；数据队列延迟到首个数据帧创建。
- **状态**：已实施（2026-11-09）。

### D12 入站映射监听地址与无认证代理

- **结论**：映射增加“监听地址”配置，默认保持 `0.0.0.0`（不破坏现有部署）。
- **空密码 socks5**：允许启动（保持现状），但启动时必须打醒目 WARN，说明该映射是无认证开放代理。
- **否决**：空密码直接拒绝启动（可能破坏现有受信网络内的无认证映射）。
- **状态**：已实施（2026-11-09）。

### D13 反向注册的端口与鉴权

- **端口范围**：强制生效——未配置时使用默认 `20000-39999`；任何情况下拒绝小于 1024 的端口（含 PreferredPort）。
- **监听地址**：动态 listener 跟随 D12 的“监听地址”配置。
- **鉴权**：不新增 register 身份认证；能连到注册机即视为已过底层链路认证（与 D11 一致）。
- **状态**：已实施（2026-11-09，监听地址部分随 D12 的映射配置；动态 listener 暂沿用 0.0.0.0）。

### D14 健康检查：配置与运行态分离

- **结论**：健康状态从组配置中剥离，单独维护运行态表（键为“组名 + 节点名”），检查结果不再回写组配置。
- **每轮取组**：从当前配置实时读取，不使用启动快照；已删除的组不再被写回“复活”。
- **选择逻辑**：best 在存活节点中按延迟选；load-balance 只在存活节点中分配。
- **持久化**：不做。健康状态只存内存，重启后重新检查；期间界面显示“检查中”。
- **DIRECT**：作为普通成员参与选择，但必须执行同样的健康检查（直连目标）得到真实延迟，不再用 0 占位。
- **REJECT**：永远排除，不参与选择。
- **检查预算**：使用全局 context 与全局截止时间；每个节点使用自身声明的超时（htunnel 35s，其余 5s），槽位等待不再固定 5s。
- **清理**：组或节点被删除时同步清理运行态条目，避免无界增长。
- **状态**：已实施（2026-11-09）：健康循环每轮实时取组并按组间隔调度；健康结果不再写回组配置；
  DIRECT 在配置了检查 URL 时真实探测、REJECT 标记不可用不参与选择；运行态表已外置为进程级
  `config.healthRegistry`（组名 → 成员 key → 状态），组对象不再持有健康数据，删组时清理。

### D15 规则引擎严格化

- **保存时严格校验**：非法 CIDR/IP、缺字段、未知规则类型、非法动作，直接拒绝保存并指出出错行；不再退化为 `0.0.0.0/0` 或空匹配串。
- **匹配语义**：域名统一小写，按“相等或 `.suffix`”做标签边界匹配；CIDR/IP 严格解析。
- **类型补齐**：实现 DOMAIN 规则；未知类型一律拒绝。
- **启停表示**：改为结构化字段（`enabled`），不再用 `//` 前缀 hack；引用检查按结构化状态判断。
- **历史数据**：加载时跳过非法规则并告警，不允许因脏数据导致启动失败。
- **状态**：已实施（2026-11-09）：规则解析严格化（类型白名单、CIDR 严格解析、域名非空校验、
  `no-resolve` 兼容）；未知类型/非法规则在加载时跳过并告警，管理面保存前直接 400；
  `DOMAIN` 匹配器补齐；`DOMAIN-SUFFIX` 改为大小写无关 + 标签边界；非法 CIDR 不再误匹配；
  规则启停改用结构化 `Enabled`，引用检查不再看 `//` 前缀。

### D16 订阅刷新

- **结论**：以上游返回为准。HTTP 非 200、网络错误、解析错误才视为失败；空结果与节点变少都视为订阅方意图，不做比例阈值、不做二次确认。
- **要修的是并发一致性**：所有对 `ActiveMember` 的写入统一走 `SetActiveMember`（带 `selMu`），删除各处直接赋值（`main.go:2845`、`admin/admin.go:3208`、`:3599-3602`）。
- **回退分支一致**：网络失败走数据库回退时，与成功分支执行相同的组重建与 ActiveMember 校验逻辑。
- **状态**：已实施（2026-11-09）。

### D17 mesh 选路

- **结论**：选路走已有查找结构，不再每包整表拷贝。
- **静态优先**：命中静态路由即直接使用，不再进入动态哈希选择；动态路径只在无静态条目时生效。
- **语义一致**：实现必须与配置/文档中的“静态优先”一致。
- **状态**：已实施（2026-11-09）：数据面直接读原子替换的不可变路由表（新增 `RouteTableRoutes()`，
  不再每包 `make+copy`）；选路先按来源分池，静态可用时只在静态池内做 sticky/哈希，动态仅在无静态时生效。

### D18 DNS 正确性与防伪造

- **校验应答**：转发响应必须校验来源地址与事务 ID，不匹配直接丢弃，防止缓存污染与伪造应答。
- **非 A 查询**：按 qtype 正确响应（返回 NODATA 或转发上游），不再用 A 记录冒充。
- **状态**：已实施（2026-11-09）：转发响应校验 QR 位、来源必须是本域实际查询的上游 GIP、TXID 必须命中
  等待中的查询，任一项不符即丢弃（不写缓存）；非 A/AAAA 查询本地直接返回 NODATA，转发的非 A 应答
  原样回传并还原客户端 TXID；缓存只存通过校验的 A 记录。

### D19 Windows TUN 清理与适配器生命周期

- **严格作用域**：删除适配器按“名称 + LUID”匹配；残留路由清理必须带 LUID 过滤，禁止全表按前缀/下一跳盲删。
- **适配器生命周期**：`StopTUN` 主动删除 `phaethontun` 适配器；TUN 被禁用或不可用时也执行一次带标记的清理。
- **状态持久化**：物理网卡 weak-host 原值、DNS 原值写入 state 文件，崩溃后可恢复。
- **状态**：已实施（2026-11-09）。

### D20 Linux / macOS TUN 清理加固

- **Linux 标记化**：自建路由带协议号标记；iptables 使用专用链 + comment；清理只按标记匹配，不再靠 `tun*` + IP 猜。
- **Linux 精确回滚**：只回滚真正修改过的 sysctl（非 bypass 模式不得写 0）；排除路由在崩溃后被清理；iptables 使用实际接口名，不再硬编码 `tun0`。
- **Linux DNS**：`resolv.conf`、`resolvectl`、`nmcli` 三条路径统一备份与恢复，停止时使用启动时记录的接口名。
- **macOS DNS**：原始 DNS 写入 state 文件，崩溃后可恢复；启动时检测并修复被污染的旧值。
- **macOS 抢占**：保留现有抢占行为（不改用户可见行为），但把外来路由快照落盘，崩溃后可恢复。
- **状态**：已实施（2026-11-09）。Linux resolved/nmcli 原值落盘恢复、路由 `Protocol=200` 标记、macOS DNS 与抢占快照落盘均已完成；iptables 评估后维持精确元组删除，不引入专用链。

### D21 P2P 入站队列的背压与内存

- **数据帧**：入队满时直接丢弃，不在会话读循环里等待（mesh 数据是 overlay，由内层 TCP 重传兜底）。
- **控制帧**：始终优先、不丢弃，避免心跳/pong 被数据帧拖累导致误判断链。
- **内存上限**：入站队列增加按字节的预算（与出站一致的 8MB/会话），不再只有条数限制。
- **状态**：已实施（2026-11-09，全局 64MB 预算 + 非阻塞丢弃）。

### D22 反向数据连接的配对身份

- **结论**：维持现状，不引入会话 nonce/HMAC。数据连接本身已通过底层代理链路认证；只有当代理口令被多个客户端共享时才存在“合法用户互相冒充”的问题，当前部署不涉及。
- **保留的低成本改进**：配对串（dyn-xxx / address）是唯一配对凭据，日志与后台列表不做完整展示（脱敏），避免日志外泄后被直接利用。
- **状态**：维持现状（仅日志脱敏）。

### D23 后台会话与凭据安全

- **密码**：改为哈希存储（bcrypt/argon2），恒定时间比较；**不做自动升级**，升级后由管理员重新设置一次密码。
- **会话密钥**：使用独立随机密钥，不再复用 admin token。
- **Cookie**：加 `Secure`，缩短有效期；改密后旧会话立即失效（会话版本号或签名密钥轮换）。
- **接口脱敏**：`/api/config/admin` 等接口不再返回密码、token、私钥原文。
- **密码重置**：不在本次改动范围，由管理员升级后自行处理（不新增 CLI 入口）。
- **状态**：已实施（2026-11-09）。

### D24 SSH 连接池

- **回收判定**：改为引用计数——连接被引用期间不回收，用完（引用归零）才进入空闲回收。
- **并发建连**：同一目标只允许一个在建请求（singleflight），其余等待复用结果，避免并发重复握手。
- **锁范围**：TCP/TLS/认证握手移出全局池锁，锁只保护 map 读写。
- **状态**：已实施（2026-11-09）。引用计数在取用连接时即持有，建连/拨号失败归还；同目标 singleflight。

### D25 P2P 协议版本兼容

- **结论**：最低版本协商 + 能力位。握手交换各自支持的版本范围与能力，取双方都能用的版本；新帧/新特性按对端能力启用。
- **不永久放弃**：协商失败或对端版本确实过低时，保持周期性探测与重连，不拉黑、不标记为永久不可用；对端升级后应自动恢复。
- **状态**：已实施（2026-11-09）。gossip 携带版本范围与能力位；动态 lane 按协商版本 + `stripe` 能力启用；
  协商结果进入 `/api/p2p`。对端未声明能力（旧构建）按不限制处理。

### D26 数据面写 TUN 的队头阻塞

- **结论**：保持单协程串行写 TUN（设备本身只支持串行），但入队分两个优先级：控制/DNS/ICMP 优先，TCP 数据次之。
- **调度**：每轮先清空高优先级队列，再按配额取数据队列，保证低优先级不被饿死。
- **状态**：已实施（2026-11-09）：writeLoop 改为双队列——ICMP/DNS(udp:53)/未知包走高优先级并整队先写，
  TCP 与其他数据走低优先级、每轮最多 32 个再回到读循环（让新到的高优先级包尽快被处理）；
  总积压上限 2048，超出部分留在 channel endpoint 形成背压；退出时统一 DecRef 防止泄漏。

### D27 mesh 网段选路失败

- **保持现状**：非 mesh 目的地（公网/本地服务）落到本地 forwarder 是设计行为，不改。
- **仅处理这一类**：目标属于 mesh 网段、但当前无可用路径（链路断/未收敛）时，显式丢弃并计数，回 ICMP 不可达，让上层尽快失败重试。
- **状态**：已实施（2026-11-09）：RouteSelector 新增 Branch 4a（mesh 网段内无路由 → 显式丢弃、不可缓存）；
  `Netstack.IsMeshUnreachable` 供 TUN 读循环判定，命中后计数（`MeshUnreachableDrops`）并回
  ICMP type 3 code 1（新 `GenerateICMPDestUnreachable`），非 mesh 目的地的本地转发行为不变。

### D28 FakeIP 分配与淘汰

- **淘汰策略**：**LRU 覆盖最不活跃**。用双向链表维护活跃度，命中时前移，淘汰时取尾部，O(1)。
- **数据库写入**：移到锁外异步写，避免写事务落盘拖慢所有 DNS 解析。
- **回调**：`onChange` 移出锁外调用，避免回调重入查池造成死锁。
- **容量校验**：初始化时检查“池大小 − 保留地址数 > 0”，否则视为配置错误直接报错；运行期不再需要“全部保留地址”的兜底分支。
- **状态**：已实施（2026-11-09）：改为 LRU 淘汰（`container/list`，命中前移、耗尽时淘汰尾部并复用地址）；
  数据库写入改为后台单协程队列（队列满时回退同步写）；`onChange` 一律在锁外调用；
  构造时校验可分配地址数，为 0 则池禁用并报错。

### D30 P2P 包缓存写入

- **大小上限**：单包限制大小，超过直接拒绝。
- **校验**：记录并校验 SHA256；接收端校验失败不落盘。
- **临时文件**：每个 key 使用唯一临时文件名，写完原子重命名，避免并发写互相覆盖。
- **状态**：已实施（2026-11-09）：单文件上限 128 MiB（超限直接拒绝，不落盘）；写入时同步计算 SHA256
  并落 `.sha256` sidecar；`SeedOwnBinary` 与 `StoreFromReader` 均改用 `os.CreateTemp` 唯一临时文件 +
  `Sync` + 原子重命名，并发写互不覆盖。

### D31 代理凭据存储

- **结论**：暂缓。字段级加密收益取决于部署环境，`data` 目录由管理员独占、备份受控时意义有限；不引入主密钥管理成本。
- **SSH 主机指纹校验**：随本项一并暂缓。
- **状态**：暂缓（不改）。

---

## 九、直接修复清单（无需决策）

以下为已核实的确定性 bug，实施阶段直接修，不再单独决策：

| # | 位置 | 问题 |
|---|---|---|
| 1 | `tun/manager.go:258-259` | `AddMeshRoute` 自递归，调用即栈溢出 |
| 2 | `main.go:1937-1970` | 对未 `Init()` 的组调用 `SetHealthImmediate`，nil map 写入 panic |
| 3 | `server/trojan.go:49` | 认证失败日志打印口令 SHA224，日志泄露可离线冒用 |
| 4 | `server/trojan.go:400-407` | 成功读取 UDP 回包后被误判为 closed 并 return，首个回包即终止读循环 |
| 5 | `dialer/dialer.go:448`、`socks5.go:217`、`htunnel.go:213/400/425` | 日志格式化参数与占位符数量不匹配 |
| 6 | `mesh/link_nic.go:185-194` | 发送失败只 `continue`，不计数 |
| 7 | `mesh/ipip_tunnel.go:37` | 每包打 Info 日志，应降为 Debug |
| 8 | `db/configstore.go:602` | `make([]uint64, 0, len(order)-1)` 在 order 为空时 panic |
| 9 | `mesh/fakeip.go:255` | `ip.To4()` 为 nil 时 panic |
| 10 | `p2p/stripe_transport.go:594/620` | 关闭时未归还池化缓冲 |
| 11 | `mesh/netstack.go:1943/2023` | 非 IPv4 / 无设备丢弃无分类计数 |
| 12 | `handler/connection_handler.go:284-293` | `localNodeDomain` 第二分支不可达（死代码） |
| 13 | `admin/templates/setup.html:417` | 期望 `data.proxies`，接口返回数组 |
| 14 | `mesh/stall_monitor.go:137-151` | 探针在监控协程内同步执行，阻塞采样（建议异步） |
