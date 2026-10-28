# JF 控制台访问失败问题跟踪

**创建时间**: 2026-10-28
**状态**: 诊断中
**优先级**: 高

## 问题描述

从本地 Mac 通过 QG 旁路网关访问 https://jf.phn 失败，DNS 解析正常但 TCP 连接超时。

**测试场景**：
- 本地 Mac (192.168.1.88) → QG 旁路网关 (192.168.1.101) → mesh 网络 → JF (100.2.0.14)
- 配置：gateway=192.168.1.101, DNS=100.0.0.3

## 核心流程（正确理解）

```
本地 Mac curl https://jf.phn
  ↓
DNS: 100.0.0.3 → 100.2.0.14 ✓
  ↓
路由: 100.0.0.0/8 → 192.168.1.101 (QG) ✓
  ↓
QG TUN: 进入 tun0 → gVisor netstack
  ↓
QG RouteSelector: 决策路由到 JF
  ↓
QG mesh P2P: 发送到 JF
  ↓
JF HandleMeshFrame: 接收数据包
  ↓
JF gVisor: LOCAL_DELIVERY → tcp.Forwarder
  ↓
JF adminHandler: 处理请求
  ↓
响应原路返回
```

## 已确认事实（日志实锤）

### ✅ 1. DNS 解析正常
**证据**：
```bash
$ nslookup jf.phn 100.0.0.3
Name: jf.phn
Address: 100.2.0.14
```
**时间**: 2026-10-28 14:38
**结论**: DNS 解析工作正常，jf.phn → 100.2.0.14

### ✅ 2. 本地 Mac 路由配置正确
**证据**：
```bash
$ networksetup -getinfo Wi-Fi
IP address: 192.168.1.88
Router: 192.168.1.101

$ scutil --dns
nameserver[0] : 100.0.0.3

$ netstat -rn | grep 100
100  192.168.1.101  UGSc  en0
```
**时间**: 2026-10-28 14:38
**结论**: 本地 Mac 配置正确，100.0.0.0/8 路由到 QG

### ✅ 3. JF TCP Forwarder 工作正常
**证据** (JF 日志 `/home/layer4/layer4-deploy/data/logs/phaethon.log`):
```
[2026-10-28 14:25:56] [INFO] [netstack] [TCP-DEBUG] acceptTCP goroutine started, registering TCP forwarder...
[2026-10-28 14:25:56] [INFO] [netstack] [NETSTACK] TCP forwarder registered for protocol 6
[2026-10-28 14:26:05] [INFO] [netstack] [TCP-DEBUG] TCP forwarder CALLBACK entered local=100.2.0.14:443 remote=100.0.0.3:37160
```
**时间**: 2026-10-28 14:25-14:26
**结论**: JF 的 tcp.Forwarder 已注册且回调被调用，工作正常

### ✅ 4. JF 本地通过 SOCKS5 代理访问成功
**证据**：
```bash
$ curl -k --max-time 10 --socks5-hostname localhost:1080 https://jf.phn/api/packages
{"packages":[...]}  # 返回完整 JSON 数据
```
**时间**: 2026-10-28 14:35
**结论**: mesh 环回路径完全通畅：SOCKS5 → mesh P2P → tcp.Forwarder → adminHandler

### ✅ 5. QG ↔ JF P2P 连接活跃
**证据** (QG 日志 `/root/data/logs/phaethon.log`):
```
[2026-10-28 14:42:21] [DEBUG] [mesh] HandleMeshFrame from jf: src=100.2.0.14 dst=100.0.0.3 proto=6 TTL=64 len=1472
[2026-10-28 14:42:21] [DEBUG] [linknic] [LINKNIC-INBOUND] linkNIC=7 peer=jf len=1472 proto=6 src=100.2.0.14 dst=100.0.0.3
[2026-10-28 14:42:21] [INFO] [netstack] writePacket ENTRY: src=100.0.0.3 dst=100.2.0.14 proto=6 needIPIP=false nicID=7
```
**时间**: 2026-10-28 14:42
**结论**: QG 接收来自 JF 的数据包并发送响应，P2P 连接正常

### ✅ 6. QG OS 路由表包含 broad routes
**证据**：
```bash
$ ip route | grep -E '0.0.0.0|128.0.0.0'
0.0.0.0/1 dev tun0 
128.0.0.0/1 dev tun0 
```
**时间**: 2026-10-28 14:42
**结论**: QG 有覆盖整个 IPv4 空间的路由，100.2.0.14 应该通过 tun0 转发

### ✅ 7. 从本地 Mac 发起的连接进入 QG gVisor 并正确路由
**证据** (QG 日志，14:48:42):
```
[MESH-TRACE] stage=tun_preinject key=tcp:100.2.0.14:443:1855245653 kind=tcp_syn src=192.168.1.88:63759 dst=100.2.0.14:443
[ROUTE-SELECTOR] FindRoute localAddr= remoteAddr=100.2.0.14 inputNIC=0 -> decision: EgressNIC=7 NeedIPIP=false LocalStack=false
forwardUnicastPacket post-FindRoute: src=192.168.1.88 dst=100.2.0.14 proto=6 ttl=62 nicID=7 needIPIP=false
[LINKNIC-WRITEPACKETS] linkNIC=7 peer=jf len=64 version=4 proto=6 src=100.0.0.1 dst=100.2.0.14
[MESH-TRACE] stage=p2p_enqueued key=tcp:100.2.0.14:443:1855245653 kind=tcp_syn src=100.0.0.1:63759 dst=100.2.0.14:443
```
**时间**: 2026-10-28 14:48:42
**结论**: 
- 本地 Mac 的数据包进入 QG 的 tun0 接口
- RouteSelector 正确决策：EgressNIC=7 (link_jf), LocalStack=false
- 数据包通过 P2P 发送到 JF
- **但是 JF 没有收到该数据包**（JF 日志中没有对应的 HandleMeshFrame）

### ✅ 8. 从 QG 本地发起的连接不经过 gVisor
**证据** (QG 日志，14:49:32-35):
```bash
# 从 QG 本地运行 curl 到 100.2.0.14
$ curl -k --max-time 3 --resolve jf.phn:443:100.2.0.14 https://jf.phn/
# 结果：Connection timed out
# 日志：14:49:32-35 期间没有任何 gVisor 日志
```
**时间**: 2026-10-28 14:49:32-35
**结论**: 
- QG 本地进程发起的连接没有进入 gVisor netstack
- 可能使用了 OS 网络栈直接发送，绕过了 tun0
- 这解释了为什么从 QG 本地无法访问 mesh 地址

### ✅ 9. JF 收到了连接但 HandleTCPConn 回调卡住
**证据** (JF 日志 `/home/layer4/layer4-deploy/data/logs/phaethon.log`):
```
[2026-10-28 14:49:07] [INFO] [TCP-DEBUG] TCP forwarder CALLBACK entered local=100.2.0.14:443 remote=100.0.0.1:63759
[2026-10-28 14:49:07] [INFO] [HANDLECONN-LAUNCH] [100.0.0.1:63759->100.2.0.14:443] launching goroutine for HandleTCPConn callback
[2026-10-28 14:49:07] [INFO] [HANDLECONN-LAUNCHED] goroutine launched successfully
[2026-10-28 14:49:07] [INFO] [HANDLECONN-GOROUTINE] goroutine STARTED
[2026-10-28 14:49:07] [INFO] [HANDLECONN-CALL] calling HandleTCPConn callback now
# 注意：没有 HANDLECONN-RETURN 日志！
```

**QG 侧数据流** (QG 日志):
```
[14:49:47] QG → JF: TCP FIN (seq=1855247472)
[14:49:57] JF → QG: TCP ACK (seq=2596600357, ack=1855245654)
[14:49:57] QG → JF: TCP FIN (seq=1855247472)
[14:50:11] QG → JF: TCP DATA (seq=1855245654, len=1500)
```

**时间**: 2026-10-28 14:49:07 - 14:50:11
**结论**: 
- JF 的 tcp.Forwarder 收到了来自 100.0.0.1:63759 的连接（SNAT 后的本地 Mac）
- HandleTCPConn 回调被调用但**卡住了，没有返回**
- QG 侧有数据流动（FIN、ACK、DATA），说明 TCP 连接本身是工作的
- **但是 JF 的 HandleTCPConn 回调没有处理数据**，导致应用层无法收到响应
- 这解释了为什么 curl 超时：TCP 连接建立了但 HTTP 请求无法完成

**根因定位**: 
- 问题在 JF 的 HandleTCPConn 回调实现
- 可能是 adminHandler.ServeConn() 卡住
- 可能是 fakeIP.LookupDomain() 失败
- 可能是 localNodeDomain 判断逻辑有问题

### ✅ 10. P2P 传输层工作正常
**证据** (QG 日志):
```
[14:49:07] stage=p2p_write_start key=tcp:100.2.0.14:443:1855245653 kind=tcp_syn src=100.0.0.1:63759 dst=100.2.0.14:443
[14:49:07] stage=p2p_write_ok key=tcp:100.2.0.14:443:1855245653 kind=tcp_syn (重复多次，可能是重传)
[14:49:07] stage=p2p_write_start key=tcp:100.2.0.14:443:1855245654 kind=tcp_ack
[14:49:07] stage=p2p_write_ok key=tcp:100.2.0.14:443:1855245654 kind=tcp_ack
```
**时间**: 2026-10-28 14:49:07
**结论**: 
- SYN 包和 ACK 包都成功通过 P2P 发送
- P2P 传输层工作正常
- **问题不在 P2P 层，而在 JF 的 HandleTCPConn 回调处理层**

### ✅ 11. JF 的 HandleTCPConn 回调被调用但从未进入函数体（核心问题）
**证据** (JF 日志):
```
[14:49:07] [netstack] [HANDLECONN-LAUNCH] [100.0.0.1:63759->100.2.0.14:443] launching goroutine
[14:49:07] [netstack] [HANDLECONN-LAUNCHED] goroutine launched successfully
[14:49:07] [netstack] [HANDLECONN-GOROUTINE] goroutine STARTED
[14:49:07] [netstack] [HANDLECONN-CALL] calling HandleTCPConn callback now
# 注意：没有 CALLBACK-ENTRY 日志！
# 注意：没有 HANDLECONN-RETURN 日志！
```

**同时有多个连接被卡住**:
```
[14:49:07] HANDLECONN-CALL for 100.0.0.1:63759->100.2.0.14:443
[14:49:07] HANDLECONN-CALL for 100.0.0.1:63760->100.2.0.14:443
[14:49:07] HANDLECONN-CALL for 100.0.0.1:63764->100.2.0.14:443
[14:49:07] HANDLECONN-CALL for 100.0.0.1:63762->100.2.0.14:443
```

**时间**: 2026-10-28 14:49:07
**结论**: 
- HandleTCPConn 回调被调用（有 HANDLECONN-CALL 日志）
- **但回调函数体从未执行**（没有 CALLBACK-ENTRY 日志）
- 多个连接同时被卡住
- **这是核心问题**：回调函数被调用但没有进入函数体

**可能原因**:
1. JF 的二进制文件过旧，没有最新的回调代码
2. 回调函数指针为 nil 或无效
3. goroutine 调度问题导致回调无法执行
4. 内存或栈损坏导致函数调用失败

**下一步**:
1. 检查 JF 二进制文件的版本/构建时间
2. 重新部署最新的 JF 二进制
3. 如果问题仍然存在，添加更多诊断日志

## 待确认问题清单

### 问题 1：从 QG 本地发起的新连接是否进入 gVisor？

**假设**：curl 从 QG 本地发起连接时，数据包可能没有进入 gVisor netstack

**验证结果**: ✅ 已验证
- 从 QG 本地运行 curl 到 100.2.0.14
- 14:49:32-35 期间没有任何 gVisor 日志
- 连接超时

**结论**: ❌ **QG 本地进程的连接不经过 gVisor**
- QG 本地进程使用 OS 网络栈，绕过了 tun0/gVisor
- 这是预期行为：OS 本地进程产生的流量不经过 TUN 设备
- 但这不是问题根因，因为最终目标是从本地 Mac 访问，不是从 QG 本地

**当前状态**: ✅ 已验证（非根因）

### 问题 2：从本地 Mac 发起的连接是否到达 JF？

**假设**：数据包从本地 Mac 发出，经过 QG 旁路网关，进入 gVisor，被路由到 JF，但 JF 没有收到

**验证结果**: ✅ 已验证
- 从本地 Mac 运行 curl 到 jf.phn (14:48:42)
- QG 日志显示：
  - 数据包进入 tun0: `stage=tun_preinject src=192.168.1.88:63759 dst=100.2.0.14:443`
  - RouteSelector 正确决策: `EgressNIC=7, LocalStack=false`
  - 数据包发送到 P2P: `stage=p2p_enqueued src=100.0.0.1:63759 dst=100.2.0.14:443`
- JF 日志显示：
  - **没有收到该 SYN 包**（搜索 192.168.1.88, 63759, tcp_syn 均无结果）

**结论**: ❌ **数据包从 QG 发送但 JF 未收到**
- QG 的 P2P 层确实发送了数据包（有 p2p_enqueued 日志）
- 但 JF 的 P2P 层没有收到该数据包
- 可能原因：
  1. P2P 连接在发送时已经断开或不可用
  2. P2P 发送失败但没有日志记录
  3. 数据包在 P2P 传输过程中丢失

**当前状态**: ✅ 已验证（定位到 P2P 传输层问题）

### 问题 3：QG 的 RouteSelector 是否有到 JF subnet 的路由？

**验证结果**: ✅ 已验证
- RouteSelector 正确决策：`EgressNIC=7, NeedIPIP=false, LocalStack=false`
- 数据包被正确路由到 JF 的 LinkNIC

**当前状态**: ✅ 已验证（路由正常）

### 问题 4：gVisor iptables 是否丢弃了数据包？

**验证结果**: ✅ 已验证
- 数据包通过了 gVisor 的 iptables 检查（有 forwardPacketWithRoute post-CheckForward 日志）
- iptables 没有丢弃数据包

**当前状态**: ✅ 已验证（iptables 正常）

### 问题 5：JF 的 HandleTCPConn 回调为什么卡住？（新发现的核心问题）

**假设**：HandleTCPConn 回调在处理连接时卡住，导致无法处理数据

**需要验证**：
- [ ] 检查 HandleTCPConn 回调的实现代码
- [ ] 查看 adminHandler.ServeConn() 是否阻塞
- [ ] 检查 fakeIP.LookupDomain() 是否失败
- [ ] 检查 localNodeDomain 判断逻辑
- [ ] 添加更详细的日志定位卡住的位置

**诊断方法**：
1. 查看 tun/engine.go 中 handleConn 的实现
2. 检查 adminHandler.ServeConn() 的代码路径
3. 在关键位置添加日志
4. 重新部署 JF 并测试

**当前状态**: ❌ 未验证（核心问题）

## 诊断日志位置

| 环境 | 日志路径 | 用途 |
|------|---------|------|
| QG | `/root/data/logs/phaethon.log` | 主日志，包含 gVisor、mesh、route 等模块日志 |
| JF | `/home/layer4/layer4-deploy/data/logs/phaethon.log` | JF 主日志 |
| 本地 Mac | 终端输出 | curl 命令输出 |

## 诊断步骤计划

### 阶段 1：验证 P2P 传输层（优先级：高）✅ 已完成
1. ✅ 从本地 Mac 发起连接到 jf.phn
2. ✅ 确认数据包进入 QG 的 gVisor
3. ✅ 确认 RouteSelector 正确决策
4. ✅ 确认数据包被发送到 P2P 层
5. ❌ **发现：JF 未收到数据包**

### 阶段 2：验证 P2P 连接状态（优先级：高）
1. 检查 QG → JF 的 P2P 连接是否活跃
2. 查看 P2P 发送是否有错误日志
3. 检查是否有 p2p_write_ok 或 p2p_send_failed 日志
4. 验证 P2P stream 是否正确建立

### 阶段 3：验证 JF 接收路径（优先级：中）
1. 检查 JF 的 P2P 接收日志
2. 查看是否有 HandleMeshFrame 调用
3. 确认数据包是否到达 JF 的 P2P 层
4. 如果到达，追踪为什么没有进入 gVisor

### 阶段 4：对比现有连接（优先级：低）
1. 对比新连接（192.168.1.88:63759）和现有连接（100.0.0.3:59290）的差异
2. 现有连接能正常通信，新连接不能
3. 查找两者的路由决策、P2P 发送路径的差异

## 结论与修复

### 根因分析

**核心问题**：JF 的 HandleTCPConn 回调被调用但从未进入函数体

**证据链**：
1. ✅ 本地 Mac 数据包进入 QG gVisor 并正确路由
2. ✅ QG RouteSelector 正确决策：EgressNIC=7, LocalStack=false
3. ✅ QG P2P 层成功发送数据包到 JF
4. ✅ JF tcp.Forwarder 收到连接并调用 HandleTCPConn 回调
5. ❌ **HandleTCPConn 回调被调用但从未进入函数体**
   - 有 HANDLECONN-CALL 日志
   - 没有 CALLBACK-ENTRY 日志
   - 没有 HANDLECONN-RETURN 日志
   - 多个连接同时被卡住

**影响**：
- TCP 连接在 JF 侧建立但无法处理数据
- curl 超时因为 HTTP 请求无法完成
- 从本地 Mac 通过 QG 旁路网关访问 jf.phn 失败

### 修复方向

1. **检查 JF 二进制**：确认是否包含最新的回调代码
2. **重新部署 JF**：使用最新代码重新编译并部署
3. **添加诊断日志**：在回调调用前后添加更多日志
4. **检查 goroutine 调度**：确认是否有 goroutine 泄漏或调度问题

### 待办事项

- [ ] 检查 JF 二进制的 git commit 版本
- [ ] 对比本地代码和 JF 运行代码的差异
- [ ] 重新部署 JF 并测试
- [ ] 如果问题仍然存在，添加更详细的诊断日志

## 更新日志

- **2026-10-28 14:50**: 完成阶段 1 诊断，发现关键问题：
  - ✅ 本地 Mac 数据包进入 QG gVisor 并正确路由
  - ✅ QG 将数据包发送到 P2P 层
  - ❌ **JF 未收到数据包**（P2P 传输失败）
  - ✅ 确认 QG 本地进程不经过 gVisor（预期行为）
- **2026-10-28 14:48**: 确认本地 Mac 发起的连接进入 QG gVisor
- **2026-10-28 14:45**: 创建问题跟踪文档，记录已确认的 6 个事实和 4 个待确认问题
- **2026-10-28 14:42**: 确认 QG ↔ JF P2P 连接活跃（现有连接）
- **2026-10-28 14:38**: 确认 DNS 解析、本地 Mac 路由、QG OS 路由表正常
- **2026-10-28 14:35**: 确认 JF 本地通过 SOCKS5 代理访问成功
- **2026-10-28 14:25**: 确认 JF TCP Forwarder 工作正常
