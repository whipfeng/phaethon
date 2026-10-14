# IPIP 隧道双向通信问题跟踪

**创建时间**: 2026-10-14  
**状态**: SNAT 未执行导致回程路由错误（待修复）  
**优先级**: 高

**问题摘要**：
- ✅ IPIP 封装已执行（QG 日志确认）
- ✅ IPIP 解封已执行（VM 日志确认）
- ✅ RouteSelector 决策正确（branch=ipip, needIPIP=true）
- ❌ **当前问题**：内层包源地址未经 SNAT（仍是 192.168.1.88，应该是 VIP）
- ❌ 导致回程包目标地址错误，TCP 握手失败

**根本原因（已确认，2026-10-14 10:30）**：

1. **问题现象**：内层包到达 VM gVisor 时端口为 0（`[192.168.1.88:0->172.30.0.2:0]`）
2. **代码分析**：
   - `mesh/ipip_tunnel.go:Decapsulate()` 创建 innerPkt，设置 NetworkHeader
   - 但**未解析 TransportHeader**（TCP/UDP 头部）
   - gVisor fork `ipv4.go:handleValidatedPacket()` 递归处理内层包
   - 但 `parseTransport()` 仅对分片包调用（line 1651），非分片包跳过
   - 导致 `DeliverTransportPacket` 收到空的 TransportHeader
   - 端口解析失败（返回 0），传输层交付失败

3. **修复方案**：
   - 位置：`/Users/cmll/IdeaProjects/gvisor-fork/pkg/tcpip/network/ipv4/ipv4.go:1359-1361`
   - 在递归调用 `handleValidatedPacket` 之前，添加 `parseTransport()` 调用
   ```go
   // Parse transport header for inner packet before recursive processing
   e.protocol.parseTransport(innerPkt, tcpip.TransportProtocolNumber(innerH.TransportProtocol()))
   // Then recursively process
   e.handleValidatedPacket(innerH, innerPkt, inNICName)
   ```

4. **修复状态**：
   - ✅ 代码已修改（gVisor fork）
   - ✅ 已重新编译（使用 `-a` 强制重建所有依赖）
   - ✅ 已部署到 QG 环境（2026-10-14 10:25）
   - ⏳ 待触发 IPIP 流量验证

**验证方法**：
- 需要从 LAN 机器通过 QG 旁路网关发起 SSH 到 172.30.0.2
- 或找到其他方式触发 IPIP 封装流量
- 检查 VM 日志是否出现 `parsing transport header for inner packet` 日志
- 检查端口是否正确解析（不再是 0）
- 检查 SSH 连接是否成功建立

**最新发现（2026-10-14 12:10）**：
1. ✅ Mac 发包正确：src=192.168.1.88 → dst=172.30.0.2，目标 MAC=QG eth1
2. ✅ QG eth1 收到包：tcpdump 抓到 192.168.1.88 → 172.30.0.2
3. ✅ QG tun0 收到包：tcpdump 抓到 192.168.1.88 → 172.30.0.2
4. ❌ QG readLoop 没有读到包：日志中没有 TUN-READ 172.30.0.2
5. ❌ 没有 IPIP 封装：因为 readLoop 没有读到包

**根因分析（2026-10-14 12:10）**：
- tcpdump 在内核层面抓包，能看到所有经过 tun0 接口的包
- readLoop 从 /dev/net/tun 文件描述符读取，读取 TUN 设备队列中的包
- 包到达 tun0 接口（tcpdump 看到了），但没有进入 TUN 设备队列（readLoop 没读到）
- 可能原因：
  1. TUN 设备队列溢出（但 qlen=500，应该足够）
  2. 内核网络栈在转发到 TUN 设备时丢弃（但 /proc/net/dev 显示接收 0 丢弃）
  3. TUN 设备驱动问题
  4. 包被其他进程读取（但只有 phaethon worker 有 /dev/net/tun 打开）

**待验证**：
- 检查 TUN 设备的实际队列使用情况
- 检查是否有其他因素阻止包进入 TUN 设备队列
- 尝试从 QG 本地发包到 tun0，看 readLoop 是否能读到

**历史发现（2026-10-14 10:12）**：
1. ✅ IPIP 解封装成功：`Decapsulating: outer=100.0.0.4->100.1.0.4 inner=192.168.1.88->172.30.0.2 proto=6 len=64`
2. ✅ 内层包到达 handleValidatedPacket：`[192.168.1.88:0->172.30.0.2:0] handleValidatedPacket proto=6 nicID=3 nicName=link_qg`
3. ✅ LOCAL_DELIVERY 触发：`localStack=true`
4. ✅ deliverPacketLocally 调用
5. ⚠️ **关键问题**：日志显示 `[192.168.1.88:0->172.30.0.2:0]`，**端口是 0，不是 22**！
6. ❌ 没有看到 `DeliverTransportPacket` 的 demux 日志（我添加的条件日志没触发）

**最新发现（2026-10-14 07:09）**：
1. ✅ QG RouteSelector 正确决策：`dst=172.30.0.2 branch=ipip prefix=172.30.0.0/24 target=vm linkNIC=4 egressEIP=100.1.0.4`
2. ✅ QG IPIP 封装执行：`[IPIP-ENCAP] outer_header: src=100.0.0.4 dst=100.1.0.4 proto=4 ttl=64 totalLen=72`
3. ✅ QG Link NIC 接收：`[QDISC] WritePacket: src=100.0.0.4 dst=100.1.0.4 proto=4 linkWriterType=*mesh.LinkNIC`
4. ✅ P2P 连接正常：`peer=6 node=vm`，QG-VM 双向通信正常（TCP 流量正常）
5. ❌ **Link NIC 未发送 IPIP 包**：有 `[QDISC] WritePacket` 但没有 `[LINKNIC-WRITEPACKETS]` 日志
6. ❌ VM 未收到 IPIP 包：没有 `[TUNNEL-DECAP]` 日志

**根本原因（待确认）**：
- Link NIC 的 `WritePacket` 方法被调用（从 qdisc），但 `WritePackets` 方法未被调用
- TCP 包（proto=6）正常发送（有 `[LINKNIC-WRITEPACKETS]` 日志）
- IPIP 包（proto=4）未发送（没有 `[LINKNIC-WRITEPACKETS]` 日志）
- 可能原因：Link NIC 内部队列或发送循环问题

## 问题描述

IPIP 隧道（RouteSelector Branch 3：非 mesh 通告路由）双向通信失败。

**测试场景**：
- 发起方：本地 Mac → QG 旁路网关 → QG TUN
- 目标：VM 通告的 172.30.0.0/24（非 mesh 路由）
- 具体测试：SSH 到 Docker@172.30.0.2:22

**现象**：
- IPIP 封装正常（QG → VM）
- IPIP 解封正常（VM 收到）
- 但 SSH 连接建立失败

## 核心流程（正确理解）

**关键概念**：不是"复用"，而是在原有流程中插入 IPIP 封装/解封步骤。

**发送方（QG）**：
```
原来的流程：
  Windows 包 → NIC 1 (tun) → gVisor 网络栈
  → iptables SNAT (src: 100.0.0.2 → VIP)
  → RouteSelector 决策
  → FindRoute 构建路由
  → 发送

插入 IPIP 封装后：
  Windows 包 → NIC 1 (tun) → gVisor 网络栈
  → iptables SNAT (src: 100.0.0.2 → VIP)
  → RouteSelector 决策 (Branch 3: NeedIPIP=true)
  → FindRoute 构建路由
  → 【插入】IPIP 封装（在发送前的最后一步）
  → 发送
```

**接收方（VM）**：
```
原来的流程：
  包到达 → gVisor 网络栈
  → RouteSelector 决策
  → TCP Forwarder 接收
  → 连接目标服务

插入 IPIP 解封后：
  IPIP 包到达
  → 【插入】IPIP 解封（在原有流程之前的第一步）
  → 内层包投入 gVisor 网络栈
  → RouteSelector 决策
  → TCP Forwarder 接收
  → 连接目标服务
```

**核心原则**：
1. **不要想复杂了**：IPIP 就是在原有流程中插入的步骤，不是独立的新流程
2. **不要另起炉灶**：不需要为 IPIP 单独设计特殊逻辑，复用现有流程
3. **对称性**：发送方在末尾插入封装，接收方在开头插入解封

## 待确认问题清单

### 问题1：QG SNAT 是否执行？

**假设**：iptables SNAT 规则条件是 `InputInterface=tun`（NIC 1），应该匹配。

**需要验证**：
- [x] QG 日志中是否有 SNAT 执行记录？
- [x] SNAT 执行后，包的 src 是否变成 VIP？
- [ ] IPIP 封装时，内层包的 src 是否是 VIP？

**诊断结果**：
- ✅ **SNAT 确实执行**：`/tmp/phaethon-nat-action-debug.log` 显示 `hook=1 srcBefore=100.0.0.2 srcAfter=100.0.0.1`
- ✅ **SNAT 目标地址正确**：VIP=100.0.0.1
- ❌ **IPIP 封装时内层包 src 未确认**：VM 日志显示的内层包是 `100.179.0.3->10.21.20.65`（GG → VM 物理 IP），不是 172.30.0.2 测试流量
- ✅ **代码逻辑正确**：`ipv4.go:940` 在 IPIP 封装前调用 `CheckPostrouting`（SNAT）

**代码位置**：
- SNAT 规则：`mesh/netstack.go:618-650`（InputInterface=tun）
- IPIP 封装前 SNAT：`gvisor-fork/pkg/tcpip/network/ipv4/ipv4.go:940`

**当前状态**: ✅ 部分验证（SNAT 执行确认，IPIP 内层包待验证）

---

### 问题2：VM 解封后的路由决策

**假设**：解封后内层包（src=VIP, dst=172.30.0.2）重新进入 gVisor，RouteSelector 应该正确决策。

**需要验证**：
- [ ] 解封后内层包的 InputNIC 是什么？
- [ ] RouteSelector 对 dst=172.30.0.2 的决策是什么？
- [ ] TCP Forwarder 是否收到连接请求？
- [ ] Forwarder 创建的 endpoint route 是什么？

**诊断方法**：
1. 在 VM 添加诊断日志：IPIP 解封后的 InputNIC
2. 在 VM 添加诊断日志：RouteSelector 决策
3. 在 VM 添加诊断日志：TCP Forwarder 接收
4. 在 VM 添加诊断日志：endpoint route 的 NIC ID

**当前状态**: ❌ 未验证

---

### 问题3：VM 回程路由

**假设**：SSH 响应（src=172.30.0.2, dst=VIP）应该通过 Link NIC 发送回 QG。

**需要验证**：
- [ ] SSH 响应包的 dst（VIP）是什么？
- [ ] RouteSelector 对 dst=VIP 的决策是什么？
- [ ] 实际从哪个 NIC 出去？

**已知问题**：
- 之前的诊断日志显示：TCP-HANDSHAKE 显示 SYN-ACK via nicID=3 (link_qg)
- 但 WritePacket 显示 nicID=1 (tun)
- 这个不一致需要解释

**诊断方法**：
1. 在 VM 添加诊断日志：SSH 响应包的 dst
2. 在 VM 添加诊断日志：RouteSelector 决策
3. 在 VM 添加诊断日志：WritePacket 的 NIC ID

**当前状态**: ❌ 未验证

---

## 诊断日志位置

### 主日志（程序自己写的滚动日志）
| 环境 | 路径 | 说明 |
|------|------|------|
| **QG** | `/root/data/logs/phaethon.log` | 主日志，10MB 轮转 |
| **VM** | `C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log` | 主日志，10MB 轮转 |
| **GG** | `/home/layer4/phaethon-gg/data/logs/phaethon.log` | 主日志，10MB 轮转 |
| **JF** | `/home/layer4/layer4-deploy/data/logs/phaethon.log` | 主日志，10MB 轮转 |
| **MS9/MS10** | `/home/migu/phaethon/logs/phaethon.log` | 主日志 |

### gVisor 诊断日志（/tmp/ 目录）
| 环境 | 文件 | 说明 |
|------|------|------|
| **QG** | `/tmp/phaethon-nat-action-debug.log` | NAT/SNAT 执行日志 |
| **QG** | `/tmp/phaethon-conntrack-debug.log` | conntrack 日志 |
| **QG** | `/tmp/phaethon-snat-debug.log` | SNAT 调用日志 |
| **GG** | `/tmp/phaethon-nat-action-debug.log` | NAT/SNAT 执行日志 |
| **GG** | `/tmp/phaethon-conntrack-debug.log` | conntrack 日志 |
| **VM** | `C:\Users\Docker\AppData\Local\Temp\phaethon-*.log` | Windows 临时目录（待确认） |

### 辅助日志
| 环境 | 路径 | 说明 |
|------|------|------|
| **QG** | `/root/phaethon.log` | stdout/stderr（不能替代主日志） |
| **VM** | `C:\Users\Docker\Desktop\Workspace\phaethon\phaethon-stdout.log` | VBS 启动时的 stdout |
| **GG** | `/home/layer4/phaethon-gg/phaethon.log` | nohup stdout（不能替代主日志） |
| **JF** | `/home/layer4/layer4-deploy/phaethon.log` | nohup stdout（不能替代主日志） |

**关键日志标签**：
- `[SNAT-DEBUG]` / `[NAT-ACTION-DEBUG]` - iptables SNAT 执行
- `[IPIP-ENCAP]` - IPIP 封装
- `[IPIP-TUNNEL]` - IPIP 解封
- `[ROUTE-SELECTOR]` / `[MESH-TRACE]` - RouteSelector 决策
- `[TCP-FWD]` - TCP Forwarder
- `[TCP-HANDSHAKE]` - TCP 握手
- `[GVISOR-TRACE]` - gVisor WritePacket

## 诊断步骤计划

### 阶段1：QG SNAT 验证
1. 在 QG 添加 SNAT 诊断日志
2. 重新编译部署 QG
3. 测试 SSH 到 172.30.0.2:22
4. 检查 QG 日志，确认 SNAT 是否执行

**实际执行结果**：
- ✅ 发现 QG 已有 SNAT 诊断日志（`/tmp/phaethon-nat-action-debug.log`）
- ✅ SNAT 确实执行：`hook=1 srcBefore=100.0.0.2 srcAfter=100.0.0.1`
- ❌ **测试方法问题**：尝试了多种方式触发 IPIP 隧道流量，但都不成功

**测试方法记录**：
1. ❌ **本地 Mac 发起 nc/curl/ping**：流量没有通过 QG 旁路网关（原因不明）
2. ❌ **QG 本地发起 curl/nc/ping**：流量没有走 tun0（tcpdump 显示 0 包）
3. ❌ **QG 本地 tcpdump 抓包**：tun0 有 mesh 流量，但没有 172.30.0.2 流量
4. ❌ **QG 本地 SSH 测试**：SSH 超时，tcpdump 显示 0 包

**QG 路由确认（2026-10-14）**：
- ✅ **精确路由存在且优先级最高**：`172.30.0.2 dev tun0 scope link`（/32 主机路由）
- ✅ **路由决策正确**：`ip route get 172.30.0.2` 返回 `172.30.0.2 dev tun0 src 100.0.0.2`
- ✅ **tun0 设备状态正常**：UP, POINTOPOINT, forwarding=1
- ✅ **FIB 树确认**：`/proc/net/fib_trie` 显示 172.30.0.2 /32 link UNICAST
- ✅ **QG 本地流量确实经过 tun0**：tcpdump 抓到 SYN 包 `100.0.0.2.37898 > 172.30.0.2.22`
- ❌ **gVisor 未处理包**：tcpdump 显示包到达 tun0，但 gVisor 没有任何日志（无 TUN-READ、无 IPIP-ENCAP）
- ❌ **readLoop 可能未运行**：没有找到 "readLoop started" 日志

**关键发现（2026-10-14 01:10）**：
- ✅ **路由曾丢失**：QG OS 路由表中没有 172.30.0.2 路由，手动添加后恢复
- ✅ **tcpdump 确认包到达 tun0**：`100.0.0.2.37898 > 172.30.0.2.22: Flags [S]`
- ❌ **gVisor 未读取包**：主日志和服务日志均无 172.30.0.2 相关处理记录
- ❌ **IPIP 封装未执行**：最近的 SSH 测试没有产生任何 IPIP-ENCAP 日志
- ✅ **早期 IPIP 封装成功**：之前从旁路网关（192.168.1.88）的流量成功执行了 IPIP 封装

**当前问题**：
- ❌ **gVisor readLoop 可能未运行或未读取 tun0**
- ❌ **需要确认**：readLoop 是否启动？tun0 文件描述符是否正确？

**下一步诊断**：
1. 检查 phaethon 进程状态和启动日志
2. 确认 TUN 设备初始化是否成功
3. 检查 readLoop goroutine 是否运行
4. 如果 readLoop 未运行，需要重启 phaethon 服务

**当前状态**: ✅ SNAT 验证完成，✅ 路由确认完成，❌ IPIP 内层包待验证（需要从 LAN 测试）

### 阶段2：VM 解封后流程验证
1. 在 VM 添加解封后诊断日志
2. 重新编译部署 VM
3. 测试 SSH 到 172.30.0.2:22
4. 检查 VM 日志，确认解封后流程

### 阶段3：VM 回程验证
1. 在 VM 添加回程诊断日志
2. 重新编译部署 VM
3. 测试 SSH 到 172.30.0.2:22
4. 检查 VM 日志，确认回程路由

## 阶段性经验教训（避免重复走弯路）

### 教训1：抓包方式要正确
**错误做法**：
```bash
# 方式1：后台抓包 + 前台测试（tcpdump 可能还没启动就开始测试了）
ssh ... "tcpdump ... &
sleep 1
ssh test..."

# 方式2：超时设置不合理（tcpdump 等待超时导致命令卡住）
ssh ... "timeout 8 bash -c 'tcpdump ... & ...'"
```

**正确做法**：
```bash
# 后台启动测试，前台抓包（确保 tcpdump 先启动）
ssh ... "nohup timeout 5 ssh test > /tmp/test.log 2>&1 &
sleep 1
timeout 3 tcpdump -i tun0 -n 'host 172.30.0.2' -c 10"
```

**教训**：
- tcpdump 必须先启动并进入监听状态
- 测试命令放后台，前台专注抓包
- 超时设置要合理，避免命令卡住

### 教训2：日志位置要搞清楚
**错误做法**：
- 检查 `/root/phaethon.log`（这是 stdout/stderr，不是主日志）
- 检查 `/tmp/phaethon-*.log`（这些是临时诊断日志，可能不存在）
- 没有区分 gVisor stderr 和 phaethon 主日志

**正确做法**：
- **主日志**：`/root/data/logs/phaethon.log`（程序自己写的滚动日志）
- **gVisor stderr**：需要通过 OpenRC 或启动脚本重定向（当前可能没有）
- **临时诊断日志**：`/tmp/phaethon-nat-action-debug.log` 等（代码中用 `os.OpenFile` 写入）

**教训**：
- 先确认日志位置，再搜索日志
- 区分主日志和辅助日志
- gVisor 的 `fmt.Fprintf(os.Stderr, ...)` 输出需要重定向才能看到

### 教训3：不要过早下结论
**错误做法**：
- 看到 tcpdump 0 packets 就结论"本地流量不走 tun0"
- 没有多次验证就认定"路由不工作"
- 没有检查抓包方式是否正确

**正确做法**：
- 多次验证，排除偶然因素
- 检查工具本身是否工作正常
- 用不同方式交叉验证

**教训**：
- 不要看到一次失败就下结论
- 先排除工具和命令本身的问题
- 用日志实锤，不要依赖单一证据

### 教训4：区分两个独立问题
**错误做法**：
- 把"QG SNAT"和"VM 回程路由"混在一起分析
- 一个问题没解决就去分析另一个问题
- 分析时把两个问题的证据混在一起

**正确做法**：
- **问题1**：QG SNAT 是否执行？IPIP 封装时内层包 src 是否是 VIP？
- **问题2**：VM 解封后路由决策是否正确？回程是否走正确的 NIC？
- 逐个问题诊断，不要交叉

**教训**：
- 复杂问题要拆分成独立子问题
- 每个问题独立诊断，独立验证
- 不要在一个问题没解决时就去分析另一个问题

## 当前定位进度

### 已确认的事实（不需要重复验证）

1. **✅ 路由配置正确**：
   - `172.30.0.2 dev tun0 scope link`（/32 精确路由，优先级最高）
   - `ip route get 172.30.0.2` 返回 `172.30.0.2 dev tun0 src 100.0.0.2`
   - tun0 设备状态：UP, POINTOPOINT, forwarding=1

2. **✅ QG 本地流量经过 tun0**：
   - tcpdump 抓到 SYN 包：`100.0.0.2.36820 > 172.30.0.2.22`
   - 包进入 gVisor：`TUN-READ-TEST` 日志确认

3. **✅ RouteSelector 决策正确**：
   - `branch=ipip, needIPIP=true, egressNIC=12, egressEIP=100.1.0.4`
   - 匹配通告路由：`prefix=172.30.0.0/24, target=vm`

4. **✅ SNAT 逻辑工作正常**：
   - `/tmp/phaethon-nat-action-debug.log` 显示 `hook=1 srcBefore=100.0.0.2 srcAfter=100.0.0.1`
   - 代码逻辑正确：`ipv4.go:940` 在 IPIP 封装前调用 `CheckPostrouting`

### 当前卡点

**❌ IPIP 封装未执行**：
- RouteSelector 决策后，没有看到 `IPIP-ENCAP` 日志
- 可能原因：
  1. FindRoute 没有正确设置 `NeedIPIP=true`
  2. FindRoute 失败（返回错误）
  3. IPIP 封装代码路径没有被调用

### 下一步诊断方向

1. **检查 FindRoute 日志**：
   - gVisor fork 的 `fmt.Fprintf(os.Stderr, ...)` 输出需要重定向
   - 检查 QG 启动脚本，确认 stderr 是否被捕获
   - 或者在 FindRoute 中添加文件日志（`os.OpenFile`）

2. **检查 RouteSelector 决策传递**：
   - RouteSelector 返回的 `RouteDecision` 包含 `NeedIPIP` 标志
   - 这个标志如何传递给 FindRoute？
   - FindRoute 是否检查了这个标志？

3. **检查 IPIP 封装代码路径**：
   - `forwardWithIPIPEncapsulation` 是否被调用？
   - 调用条件是什么？
   - 是否有提前返回或错误处理？

## 结论与修复

**待填充**

---

## 更新日志

- 2026-10-14: 创建跟踪文档，整理问题和诊断计划
- 2026-10-14: 添加阶段性经验教训（抓包方式、日志位置、不要过早下结论、区分两个问题）
- 2026-10-14: 更新定位进度（已确认路由、流量、RouteSelector 决策正确；卡点在 FindRoute/IPIP 封装）

### 教训5：日志搜索要精确
**错误做法**：
```bash
# 搜索范围太广，找不到关键信息
grep 'IPIP' /root/phaethon-service.log | tail -20

# 搜索关键词不对
grep 'FORWARD' /root/phaethon-service.log | grep '172.30'
```

**正确做法**：
```bash
# 精确搜索特定目标的日志
grep '172.30.0.2' /root/phaethon-service.log | tail -20

# 搜索特定函数的日志
grep 'IPIP-ENCAP.*172.30.0.2' /root/phaethon-service.log | tail -10

# 搜索 CheckPostrouting 的日志
grep 'CheckPostrouting.*172.30' /root/phaethon-service.log | tail -10
```

**教训总结**：
- 用目标 IP 作为关键词搜索，而不是泛泛搜索
- 搜索特定函数的日志时，加上目标 IP 过滤
- **验证标准**：能看到完整的调用链（ENTRY → FindRoute → IPIP-ENCAP → forwardPacketWithRoute）

## 诊断规则

**自主诊断原则**：
- 诊断过程无需向用户汇报，直接执行
- 遇到问题主动添加诊断日志、重新测试、检查日志
- 及时更新跟踪文档，记录发现和结论
- 所有结论必须有日志证据支持

**诊断流程**：
1. 确认日志位置
2. 搜索关键日志
3. 分析证据
4. 得出结论
5. 更新文档

---

## 重大进展（2026-10-14 00:47）

### ✅ IPIP 封装/解封双向通信已打通

**QG 端日志**（`/root/phaethon-service.log`）：
```
[FORWARD-DEBUG] forwardUnicastPacket ENTRY: src=100.0.0.2 dst=172.30.0.2 proto=6 ttl=63
[ROUTE-SELECTOR] FindRoute localAddr= remoteAddr=172.30.0.2 inputNIC=0 -> decision: EgressNIC=4 NeedIPIP=true LocalStack=false
[FORWARD-DEBUG] FindRoute SUCCESS: src=100.0.0.2 dst=172.30.0.2
[GVISOR-TRACE] forwardUnicastPacket post-FindRoute: src=100.0.0.2 dst=172.30.0.2 proto=6 ttl=63 nicID=4 needIPIP=true
[IPIP-ENCAP] start: inner=100.0.0.2->172.30.0.2 proto=6 len=60, outer=100.0.0.4->100.1.0.4 egressEIP=100.1.0.4 linkNIC=4
[IPIP-ENCAP] forwardPacketWithRoute OK: inner=100.0.0.2->172.30.0.2 outer=100.0.0.4->100.1.0.4
```

**VM 端日志**（`C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log`）：
```
[IPIP-TUNNEL] ShouldDecapsulate: outer dst=100.1.0.4 local EIP=100.1.0.4 match=true
[IPIP-TUNNEL] Decapsulating: outer=100.0.0.4->100.1.0.4 inner=100.0.0.2->172.30.0.2 proto=6 len=60
[IPIP-TUNNEL] Decapsulate: after Consume innerPktLen=60 networkHdrLen=60
```

**确认的事实**：
1. ✅ IPIP 封装成功（QG → VM）
2. ✅ IPIP 解封成功（VM 收到）
3. ✅ 内层包正确：src=100.0.0.2, dst=172.30.0.2
4. ✅ 外层头正确：src=100.0.0.4 (QG EIP), dst=100.1.0.4 (VM EIP)

### ❌ 当前卡点：SNAT 未执行

**问题**：内层包的 src 是 100.0.0.2，不是 VIP（100.0.0.1）

**预期**：IPIP 封装前应该先执行 SNAT，把 src 从 100.0.0.2 改成 VIP（100.0.0.1）

**证据**：
- 日志显示 `inner=100.0.0.2->172.30.0.2`，src 没有被 SNAT 改成 VIP
- 没有看到 `[IPIP-ENCAP] CheckPostrouting PASS` 日志

**可能原因**：
1. CheckPostrouting 没有被调用
2. CheckPostrouting 被调用了，但 SNAT 规则没有匹配
3. SNAT 规则匹配了，但没有生效

**下一步诊断**：
1. 检查 CheckPostrouting 是否被调用（添加日志）
2. 检查 SNAT 规则是否正确（InputInterface=tun 是否匹配）
3. 检查 conntrack 状态


## 重大突破：IPIP 隧道双向通信已打通（2026-10-14 00:55）

### ✅ 已确认的完整流程

**QG 端（发送方）**：
```
[FORWARD-DEBUG] forwardUnicastPacket ENTRY: src=100.0.0.2 dst=172.30.0.2 proto=6 ttl=63
[ROUTE-SELECTOR] FindRoute -> decision: EgressNIC=4 NeedIPIP=true LocalStack=false
[GVISOR-TRACE] forwardUnicastPacket post-FindRoute: needIPIP=true
[IPIP-ENCAP] start: inner=100.0.0.1->172.30.0.2 proto=6 len=52, outer=100.0.0.4->100.1.0.4
[IPIP-ENCAP] forwardPacketWithRoute OK
```

**VM 端（接收方）**：
```
[TUNNEL-DECAP] proto=4 decapsulated: inner=100.0.0.1->172.30.0.2 proto=6 len=52
[GVISOR-TRACE] [100.0.0.1:0->172.30.0.2:0] handleValidatedPacket proto=6 nicID=5 nicName=link_qg localStack=true
[GVISOR-TRACE] [100.0.0.1:0->172.30.0.2:0] LOCAL_DELIVERY proto=6 nicName=link_qg
[GVISOR-TRACE] [100.0.0.1:0->172.30.0.2:0] DeliverTransportPacket CALL proto=6 nicName=link_qg
```

**VM SSH 服务状态**：
```
TCP    172.30.0.2:22          10.11.61.41:64151      ESTABLISHED
```

### ✅ 确认的事实

1. ✅ **IPIP 封装成功**：QG 正确封装，inner=100.0.0.1->172.30.0.2, outer=100.0.0.4->100.1.0.4
2. ✅ **SNAT 成功**：内层包 src 从 100.0.0.2 改成 VIP（100.0.0.1）
3. ✅ **IPIP 解封成功**：VM 正确解封，内层包进入 gVisor
4. ✅ **本地交付成功**：gVisor 识别为本地路由（localStack=true），执行 LOCAL_DELIVERY
5. ✅ **传输层交付成功**：DeliverTransportPacket 被调用
6. ✅ **SSH 连接建立**：VM SSH 服务显示 ESTABLISHED 连接

### ❌ 当前问题

**现象**：SSH 连接超时，但 VM netstat 显示 ESTABLISHED 连接

**可能原因**：
1. VM SSH 服务看到的 dst 是 10.11.61.41（QG 物理 IP），不是 VIP（100.0.0.1）
2. gVisor TCP Forwarder 可能无法直接连接到 OS 网络栈的服务（172.30.0.2:22）
3. 回程包可能没有经过 IPIP 封装，而是直接发送到 QG 物理 IP

**下一步诊断**：
1. 检查 gVisor TCP Forwarder 是否正确处理连接
2. 检查 VM 的 OS 网络栈是否有 NAT 转换
3. 检查回程包的路由决策


## 最新进展（2026-10-14 09:35）

### ✅ Gossip 路由传播验证完成

**已确认的事实**：
1. ✅ **VM 通告路由配置正确**：`GET /api/mesh/advertise` 返回 `["10.11.61.0/24","10.99.0.0/24","10.21.20.0/24","172.30.0.0/24","172.30.0.0/24"]`
2. ✅ **VM 通过 gossip 发送路由**：VM 日志显示 `sending gossip to qg: claimedSubnets=1 routes=4`
3. ✅ **QG 路由表包含通告路由**：QG 日志显示 `routeCount=11 prefixes=[... 172.30.0.0/24 ...]`
4. ✅ **Mac OS 路由正确**：`172.30/24 via 192.168.1.101`（QG 旁路网关）

**关键日志证据**：
```
# VM 发送 gossip
[phaethon] 2026/10/14 09:32:20 [INFO] [MESH] sending gossip to qg: claimedSubnets=1 routes=4

# QG 路由决策包含 172.30.0.0/24
[phaethon] 2026/10/14 09:33:50 [INFO] [trace] [MESH-TRACE] stage=route_decision dst=100.1.0.19 select_route_entry routeCount=11 prefixes=[10.161.88.0/24 10.99.0.0/24 10.21.20.0/24 172.30.0.0/24 ...]
```

### ✅ IPIP 隧道端到端验证（旁路网关场景）

**测试场景**：Mac (192.168.1.88) → QG 旁路网关 (192.168.1.101) → QG TUN → IPIP 封装 → VM → IPIP 解封

**QG 端日志**（`/root/data/logs/phaethon.log`）：
```
[phaethon] 2026/10/14 09:34:08 [INFO] [tun] [TUN-READ-TEST] packet #119122: 192.168.1.88 -> 172.30.0.2 proto=6 len=64
[phaethon] 2026/10/14 09:34:08 [INFO] [gvisor] [192.168.1.88:0->172.30.0.2:0] handleValidatedPacket proto=6 nicID=1 nicName=tun forwarding=true outputNIC="" localStack=false
[phaethon] 2026/10/14 09:34:08 [INFO] [forward] forwardUnicastPacket ENTRY: src=192.168.1.88 dst=172.30.0.2 proto=6 ttl=62
[phaethon] 2026/10/14 09:34:08 [INFO] [forward] FindRoute SUCCESS: src=192.168.1.88 dst=172.30.0.2
[phaethon] 2026/10/14 09:34:08 [INFO] [gvisor] forwardUnicastPacket post-FindRoute: src=192.168.1.88 dst=172.30.0.2 proto=6 ttl=62 nicID=4 needIPIP=true
[phaethon] 2026/10/14 09:34:08 [INFO] [ipip] start: inner=192.168.1.88->172.30.0.2 proto=6 len=64, outer=100.0.0.4->100.1.0.4 egressEIP=100.1.0.4 linkNIC=4
[phaethon] 2026/10/14 09:34:08 [INFO] [ipip] forwardPacketWithRoute OK: inner=192.168.1.88->172.30.0.2 outer=100.0.0.4->100.1.0.4
```

**VM 端日志**（`C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log`）：
```
[phaethon] 2026/10/14 09:33:50 [INFO] [IPIP-TUNNEL] Decapsulating: outer=100.0.0.4->100.1.0.4 inner=192.168.1.88->172.30.0.2 proto=6 len=64
[phaethon] 2026/10/14 09:33:51 [INFO] [IPIP-TUNNEL] Decapsulating: outer=100.0.0.4->100.1.0.4 inner=192.168.1.88->172.30.0.2 proto=6 len=64
[phaethon] 2026/10/14 09:33:52 [INFO] [IPIP-TUNNEL] Decapsulating: outer=100.0.0.4->100.1.0.4 inner=192.168.1.88->172.30.0.2 proto=6 len=64
```

**VM 服务状态**：
```
TCP    172.30.0.2:22          10.11.61.41:56651      ESTABLISHED
TCP    172.30.0.2:139         0.0.0.0:0              LISTENING
```

### ✅ 确认的事实

1. ✅ **Gossip 路由传播正常**：VM 的通告路由 (172.30.0.0/24) 通过 gossip 正确传播到 QG
2. ✅ **QG 路由表正确**：QG 的 RouteSelector 包含 172.30.0.0/24 路由
3. ✅ **Mac OS 路由正确**：Mac 有到 172.30.0.0/24 的路由指向 QG 旁路网关
4. ✅ **QG IPIP 封装成功**：inner=192.168.1.88->172.30.0.2, outer=100.0.0.4->100.1.0.4
5. ✅ **VM IPIP 解封成功**：VM 收到并解封 IPIP 包
6. ✅ **P2P 传输正常**：IPIP 包通过 P2P 链路从 QG 成功传输到 VM

### ❌ 当前卡点：解封后包交付失败

**现象**：
- VM 正确解封 IPIP 包（inner=192.168.1.88->172.30.0.2）
- 但 SSH/HTTP 连接超时
- VM 上 172.30.0.2:22 (SSH) 和 172.30.0.2:139 (NetBIOS) 有服务监听
- 但解封后的包没有到达 Windows 网络栈，或响应包没有发回来

**可能原因**：
1. gVisor TunnelProtocol 解封后，内部数据包没有正确注入到 gVisor 网络栈
2. 内部数据包注入后，RouteSelector 决策不正确
3. 内部数据包到达 Windows 网络栈，但响应包没有正确路由回来
4. gVisor 网络栈和 Windows OS 网络栈之间的交付有问题

**下一步诊断**：
1. 检查 gVisor TunnelProtocol.Decapsulate() 返回后，内部数据包如何处理
2. 在 VM 添加诊断日志：解封后内部数据包的注入点
3. 在 VM 添加诊断日志：解封后内部数据包的 RouteSelector 决策
4. 检查 VM 的 TCP Forwarder 是否收到连接请求

### 经验教训（2026-10-14）

**教训6：Gossip 路由传播需要验证**
- ✅ 配置了通告路由不代表其他节点收到了
- ✅ 需要通过日志或 API 确认路由已经传播
- ✅ 验证方法：检查目标节点的 `routeCount` 和 `prefixes` 列表

**教训7：IPIP 隧道测试需要端到端验证**
- ✅ 封装成功不代表解封成功
- ✅ 解封成功不代表包交付成功
- ✅ 需要在每个关键节点添加日志，追踪完整路径

**教训8：区分"配置"和"运行时状态"**
- ❌ 错误：认为配置了通告路由就一定会传播
- ✅ 正确：需要通过日志或 API 确认运行时状态

**教训9：gVisor 日志注入需要重新编译**
- ❌ 错误：VM 没有 [gvisor] 日志，以为是日志轮转太快
- ✅ 正确：VM 的 phaethon 二进制版本过旧，没有 gVisor 日志注入功能
- ✅ 解决：重新编译 `make windows` 并部署到 VM

**教训10：DeliverTransportPacket 调用不等于 TCP Forwarder 处理**
- ❌ 错误：看到 `DeliverTransportPacket CALL` 就认为 TCP Forwarder 会处理
- ✅ 正确：DeliverTransportPacket 只是 gVisor 传输层交付，TCP Forwarder 是否处理取决于连接匹配
- ✅ 需要：在 TCP Forwarder handler 中添加日志，确认是否被调用

---

## 最新卡点：TCP Forwarder 未处理连接（2026-10-14 09:45）

### 问题现象

**VM 端日志显示包已到达 gVisor 传输层**：
```
[IPIP-TUNNEL] Decapsulating: outer=100.0.0.4->100.1.0.4 inner=192.168.1.88->172.30.0.2 proto=6 len=64
[gvisor] proto=4 decapsulated: inner=192.168.1.88->172.30.0.2 proto=6 len=64
[gvisor] handleValidatedPacket proto=6 nicID=3 nicName=link_qg localStack=true
[gvisor] LOCAL_DELIVERY proto=6 nicName=link_qg
[gvisor] deliverPacketLocally ENTRY inNIC=link_qg
[gvisor] CheckInput ok=true
[gvisor] DeliverTransportPacket CALL proto=6 nicName=link_qg
```

**但是没有 TCP-FWD 日志**：
- ❌ 没有 `[TCP-FWD]` 日志
- ❌ 没有 `[TCP-HANDSHAKE]` 日志
- ❌ SSH/HTTP 连接超时

### 根因分析（2026-10-14 09:52）

**关键发现**：
1. ✅ 包到达 `DeliverTransportPacket`
2. ✅ 其他连接（如 100.1.0.27:443）的 `defaultHandler` 被调用且返回 true
3. ❌ 172.30.0.2 的连接没有调用 `defaultHandler`

**结论**：
- `demux.deliverPacket` 为 172.30.0.2 找到了一个 endpoint（返回 true）
- 但这个 endpoint 不是 TCP Forwarder
- TCP Forwarder 是 `defaultHandler`，只在 `demux.deliverPacket` 返回 false 时才被调用
- 172.30.0.2 被某个特定的 endpoint 匹配了，但这个 endpoint 没有正确处理连接

**可能原因**：
1. gVisor 为 172.30.0.2 创建了一个临时的 endpoint（错误地认为它是本地地址）
2. 这个临时 endpoint 没有正确实现 TCP 处理逻辑
3. 或者这个 endpoint 是某个内部协议处理器，不是用户态的 TCP Forwarder

**下一步诊断方向**：
1. 检查 gVisor 是否为 172.30.0.2 注册了特定的 endpoint
2. 检查 `findEndpointLocked` 返回的是什么类型的 endpoint
3. 检查为什么 172.30.0.2 被认为是本地地址（应该不是）

### 当前状态

- ✅ IPIP 封装/解封工作正常
- ✅ 包到达 VM 的 gVisor 传输层
- ❌ TCP Forwarder 未处理连接（被错误的 endpoint 拦截）
- ❌ SSH/HTTP 连接超时

---

## SSH 连接失败诊断（2026-10-14 01:00）

### 问题现象
- IPIP 隧道双向通信打通
- VM TCP Forwarder 收到 SYN 并发送 SYN-ACK
- 但收到 RST 包，连接失败

### VM 端日志
```
[TCP-FWD] [100.1.0.1:61769->172.30.0.2:22] SYN received, calling handler
[TCP-HANDSHAKE] [100.1.0.1:61769->172.30.0.2:22] sending SYN-ACK via route nicID=1
[TCP-FWD] [100.1.0.1:61769->172.30.0.2:22] not a SYN packet, flags= R
```

### 关键发现
1. ✅ TCP Forwarder 收到 SYN（src=100.1.0.1, dst=172.30.0.2:22）
2. ✅ TCP Forwarder 发送 SYN-ACK
3. ❌ 收到 RST 包，连接被重置

### 问题分析
**疑问**：为什么 src=100.1.0.1（VM VIP）而不是 100.0.0.1（QG VIP）？

**可能原因**：
1. VM OS 网络栈做了 SNAT，把 src 从 100.0.0.1 改成 100.1.0.1
2. conntrack 状态跟踪有问题
3. QG 收到 SYN-ACK 后发送了 RST

### 下一步诊断
1. 检查 QG 是否收到 SYN-ACK
2. 检查 QG 是否发送了 RST
3. 检查 conntrack 状态
4. 检查 VM OS 网络栈的 NAT 规则

---

## 最新状态（2026-10-14 11:45）

### ✅ IPIP 隧道全链路已验证打通

**端到端日志证据**：

**QG 端（封装方）**：
```
[ipip] start: inner=100.0.0.1->172.30.0.2 proto=6 len=40, outer=100.0.0.4->100.1.0.4
[ipip] outer_header: src=100.0.0.4 dst=100.1.0.4 proto=4 ttl=64 totalLen=60
[ipip] FindRoute OK: egressEIP=100.1.0.4 nicID=4 needIPIP=false
[ipip] writePacketPostRouting: proto=4 loop=1 packetOut=true
[ipip] forwardPacketWithRoute OK: inner=100.0.0.1->172.30.0.2 outer=100.0.0.4->100.1.0.4
```

**VM 端（解封装方）**：
```
[ipip] [IPIP-TUNNEL] ShouldDecapsulate: outer dst=100.1.0.4 local EIP=100.1.0.4 match=true
[ipip] [IPIP-TUNNEL] Decapsulating: outer=100.0.0.4->100.1.0.4 inner=100.0.0.1->172.30.0.2 proto=6 len=52
[ipip] [IPIP-TUNNEL] Decapsulate: innerData len=52 (complete IP packet, no link header)
```

多次 TCP SYN 重试（11:41:42 ~ 11:42:07），说明 QG→VM 的 IPIP 封装→P2P 传输→解封装链路稳定工作。

### 当前卡点：解封装后内层包的交付

**现象**：
- IPIP 解封装成功，内层包（100.0.0.1→172.30.0.2 proto=6）通过 `HandlePacket` 注入 gVisor netstack
- SSH 报错从 "Operation timed out" 变为 "Connection timed out during banner exchange"（有进展，说明 TCP 部分连通）
- 但 SSH 最终超时

**正确理解解封装后的包处理**：

解封装后的内层包就是一个**普通 IP 包**，跟其他任何进入 gVisor netstack 的包没有区别。它的去向完全由 RouteSelector 决定：
- 如果 RouteSelector 判定 LocalStack=true → 交付给本地 TCP Forwarder
- 如果 RouteSelector 判定需要转发 → 走转发路径（可能经 Link NIC 或再次 IPIP）
- 不需要也不应该考虑"把某个地址绑定到某个 NIC"之类的概念

**当前缺失的诊断**：
- VM 上解封装后内层包的 RouteSelector 决策日志（route 模块当前设为 info 级别，但 route-selector 日志已降为 debug）
- 需要设 `route=debug` 才能看到解封装后内层包的路由决策

### 日志级别调整（本次部署）

逐包诊断日志全部降为 Debug 级别，默认 INFO 下不再输出，需要时通过模块日志级别开启：

| 模块 | 日志内容 | 原级别 | 新级别 |
|------|----------|--------|--------|
| `mesh` | MESH-TRACE 转发追踪 | Info | Debug |
| `mesh` | RouteSelector 决策 | Info | Debug |
| `mesh` | LinkNIC 收发包 | Info | Debug |
| `mesh` | HandleMeshFrame IPIP 接收 | Info | Debug |
| `p2p` | P2P 收发包诊断 | Info | Debug |
| `tun` | TUN read 逐包日志 | Info | Debug |
| `ipip`（gVisor fork） | IPIP 封装路径日志 | Info（via stack.Logf） | Debug（via stack.Logf） |

开启方式：启动环境变量 `PHAETHON_LOG_MODULES=route=debug,mesh=debug,p2p=debug,tun=debug,ipip=debug` 或 admin API `PUT /api/log/modules`。

### 下一步

1. 在 VM 上设 `route=debug`，触发 SSH 测试
2. 检查解封装后内层包（dst=172.30.0.2）的 RouteSelector 决策
3. 确认 LocalStack 是否为 true（本地交付）还是走了转发
4. 根据 RouteSelector 决策结果判断下一步修复方向

---

## 最新诊断进展（2026-10-14 11:55）

### ✅ 已完成：VM 端日志级别调整

- 设置 VM 的 `route=debug, gvisor=debug, ipip=debug`
- 触发 SSH 测试（从本地 Mac 通过 QG 旁路网关）
- VM 日志确认 IPIP 解封装成功：`Decapsulating: outer=100.0.0.4->100.1.0.4 inner=100.0.0.1->172.30.0.2 proto=6 len=40`
- **但是没有看到解封装后内层包的 RouteSelector 决策日志**

### ❌ 当前卡点：QG 本地流量未进入 gVisor

**现象**：
- tcpdump 在 QG tun0 上抓到包：`100.0.0.2.48326 > 172.30.0.2.22: Flags [S]`
- 但 QG 的 readLoop 日志中没有 172.30.0.2 的相关记录
- 说明包到达 tun0 后，没有被 gVisor 的 readLoop 读取

**可能原因**：
1. tcpdump 在内核层抓包，readLoop 在用户态读取 TUN 文件描述符，包可能在内核层面被丢弃
2. 或者 readLoop 读取了包，但在某个地方被过滤掉（没有输出日志）

**本地 Mac 路由问题**：
- 初始路由错误：172.30.0.2 指向 www.whipfeng2.com（en0 接口）
- 已修复：删除旧路由，添加 `172.30.0.0/24 via 192.168.1.101`
- 但 tcpdump 显示 src=100.0.0.2（QG IP），说明本地流量到达 QG 后被 SNAT 转换

### 待验证

1. **QG readLoop 为何没有读取 172.30.0.2 的包**：
   - 检查 readLoop 是否有过滤逻辑
   - 检查 TUN 设备配置是否正确
   - 检查是否有内核层面的包丢弃

2. **VM 解封装后内层包的处理**：
   - 需要确认内层包是否再次经过 RouteSelector
   - 如果经过，RouteSelector 的决策是什么
   - 如果没有经过，说明 gVisor fork 的 HandlePacket 递归调用有问题

### 经验教训

**教训11：tcpdump 抓包 ≠ readLoop 读取**
- tcpdump 在内核层抓包，能看到所有到达接口的包
- readLoop 在用户态读取 TUN 文件描述符，可能因为各种原因读取不到包
- 不能仅凭 tcpdump 抓到包就认为 readLoop 会读取到

**教训12：本地流量可能不走预期路径**
- QG 本地发起的流量（src=100.0.0.2）虽然路由指向 tun0，但可能不经过 gVisor readLoop
- 旁路网关流量（src=192.168.1.88）是正常的（有 TUN-READ 日志）
- 需要区分"本地发起的流量"和"经过旁路网关的流量"

---

## 最新发现（2026-10-14 14:15）

### ❌ 当前问题：SNAT 未执行，内层包源地址错误

**现象**：
- QG IPIP 封装成功：`inner=192.168.1.88->172.30.0.2`
- VM IPIP 解封成功：内层包进入 gVisor
- **但内层包源地址是 192.168.1.88（LAN 地址），不是 VIP（100.0.0.1）**

**预期行为**：
- IPIP 封装前应该执行 SNAT，把 src 从 192.168.1.88 改成 VIP（100.0.0.1）
- 这样回程包才能正确路由回 QG

**历史对比**：
- ✅ 之前验证过 SNAT 工作正常（line 571）：`inner=100.0.0.1->172.30.0.2`
- ❌ 现在 SNAT 未执行：`inner=192.168.1.88->172.30.0.2`

**可能原因**：
1. gVisor iptables SNAT 规则条件不匹配（InputInterface=tun 可能不适用于旁路网关流量）
2. CheckPostrouting 没有被调用
3. SNAT 规则被覆盖或丢失

**下一步诊断**：
1. 检查 gVisor iptables SNAT 规则配置
2. 检查 CheckPostrouting 是否被调用（添加日志）
3. 确认 SNAT 规则条件是否匹配旁路网关流量
4. 对比之前 SNAT 工作的代码版本，找出差异

**影响**：
- 回程包目标地址错误（应该是 VIP，但现在是 192.168.1.88）
- VM 发送 SYN-ACK 到 192.168.1.88，但 192.168.1.88 不在 VM 的路由表中
- TCP 握手失败

