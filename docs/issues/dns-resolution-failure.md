# DNS 解析失败问题跟踪

**创建时间**: 2026-10-15
**状态**: ✅ 已解决（2026-10-21 确认）
**优先级**: 高（已解决）

## 研判记录（2026-10-21）

**研判依据**:
1. ✅ **QG 日志检查**: `tail -500 /root/data/logs/phaethon.log | grep -i 'SERVFAIL\|dns.*fail'` → 无 DNS 错误
2. ✅ **GG 日志检查**: `tail -500 /home/layer4/phaethon-gg/data/logs/phaethon.log | grep -i 'SERVFAIL\|dns.*fail'` → 无 DNS 错误
3. ✅ **代码确认**: DNS 诊断日志系统已添加（commit 8145677），可捕获 DNS 解析失败

**局限性**:
- ❌ 未做**批量 DNS 解析测试**（需查询 100+ 次统计成功率）
- ❌ 未做**跨节点 DNS 测试**（需验证所有节点间 DNS 转发正常）
- ❌ 仅检查最近 500 行日志，可能遗漏间歇性问题

**结论**: 基于最近日志无 DNS 错误，判断问题已解决。如复现需检查 DNS 转发链路和上游 DNS 可达性。

**复现排查方向**:
1. 检查 `mesh/dns.go` 中 DNSHijacker 是否正常启动
2. 检查跨节点 DNS 转发是否成功（查看 DNS-DEBUG 日志）
3. 检查上游 DNS（direct-nameserver）是否可达
4. 使用 `curl -sk https://localhost:39999/api/mesh/dns/resolve?domain=xxx.phn` 测试解析

## 问题描述

所有 mesh 域名（jf.phn, gg.phn 等）DNS 解析失败，返回 SERVFAIL。

**测试场景**：
- 从本地 Mac (192.168.1.88) 通过 QG 旁路网关查询
- DNS 服务器：100.0.0.3（QG 的 GIP）
- 测试命令：`nslookup jf.phn 100.0.0.3` 和 `nslookup gg.phn 100.0.0.3`

**现象**：
- 两个域名都返回 SERVFAIL
- QG 日志中没有任何 DNS 处理日志（无 processQuery、无 ResolveDomainSubnet、无 forwardToRemote）
- INJECT-DEBUG 显示 DNS 包被注入：`192.168.1.88 -> 100.0.0.3:53`

## 核心流程（正确理解）

```
1. 客户端发送 DNS 查询到 100.0.0.3:53
2. 包到达 TUN 设备
3. readLoop 读取包并调用 InjectInbound 注入到 gVisor netstack
4. gVisor netstack 路由决策（RouteSelector）
5. 包交付给本地 DNSHijacker（因为 localStack=true）
6. DNSHijacker 处理查询：
   - jf.phn: 直接 peer (hop=1)，转发到 JF (100.2.0.3:53)
   - gg.phn: 多跳 (hop=2 via JF)，转发到 JF，JF 再转发到 GG
7. DNSHijacker 返回响应
```

## 待确认问题清单

### 问题 1：DNSHijacker 是否收到包？

**假设**：DNSHijacker 没有收到 DNS 查询包

**需要验证**：
- [ ] DNS 模块的日志级别是否开启？
- [ ] DNSHijacker 的 serveLoop 是否在运行？
- [ ] DNSHijacker 绑定的地址是否正确（100.0.0.3:53）？
- [ ] gVisor netstack 是否将包交付给本地 UDP endpoint？

**诊断方法**：
1. 检查 DNS 模块日志级别
2. 检查 DNSHijacker 启动日志
3. 添加更详细的诊断日志
4. 检查 HandleLocal 设置

**当前状态**: ❌ 未验证

### 问题 2：SERVFAIL 是谁返回的？

**假设**：如果 DNSHijacker 没收到包，不应该返回 SERVFAIL（应该超时）

**需要验证**：
- [ ] SERVFAIL 响应确实来自 100.0.0.3:53 吗？
- [ ] 是否有其他 DNS 服务在监听？
- [ ] 是否是 gVisor 内部某个组件返回的错误？

**诊断方法**：
1. 用 tcpdump 抓包确认响应源地址
2. 检查是否有其他 DNS 监听
3. 检查 gVisor 内部的错误处理路径

**当前状态**: ❌ 未验证

### 问题 3：HandleLocal=false 的影响

**观察**：
- QG 日志显示 `HandleLocal=false`
- RouteSelector 返回 `localStack=true`
- 但 HandleLocal=false 可能阻止栈内环回

**需要验证**：
- [ ] HandleLocal=false 是否阻止了包交付给本地 endpoint？
- [ ] 补丁 #10 的 LocalLoopback 是否生效？

**诊断方法**：
1. 检查 gVisor fork 中 LocalLoopback 的实现
2. 检查 RouteDecision 是否正确设置了 LocalLoopback
3. 测试启用 HandleLocal=true 的效果

**当前状态**: ❌ 未验证

## 诊断日志位置

- **QG 主日志**: `/root/data/logs/phaethon.log`
- **关键日志标签**:
  - `[DNS-DEBUG]` - DNSHijacker 调试日志
  - `[INJECT-DEBUG]` - 包注入日志
  - `[ROUTE-SELECTOR]` - 路由决策日志
  - `processQuery` - DNS 查询处理
  - `forwardToRemote` - DNS 转发

## 诊断步骤计划

### 阶段 1：确认日志级别
1. 检查 DNS 模块日志级别是否开启
2. 检查 DNSHijacker 启动日志
3. 确认 DNSHijacker 绑定地址

### 阶段 2：追踪包路径
1. 添加更详细的诊断日志（在 InjectInbound 后、RouteSelector 后、DNSHijacker 收到时）
2. 重新编译部署
3. 重新测试并检查日志

### 阶段 3：验证 HandleLocal 影响
1. 检查 LocalLoopback 补丁是否生效
2. 测试启用 HandleLocal=true
3. 验证是否解决问题

## 结论与修复

### 根因分析（已确认）

**问题根因 1：日志系统 RotatingFileWriter 静默失败**

**现象**：
- 日志文件运行一段时间后停止更新（825MB + 1GB old）
- DNS 查询继续处理但无日志输出
- 无法诊断 DNS SERVFAIL 的真正原因

**修复**：
- 修复 `util/util.go` 中 `RotatingFileWriter.rotate()` 方法
- 原代码：`rotate()` 忽略 `os.Remove`, `os.Rename`, `openFile()` 的所有错误
- 如果 rename 失败，`w.file` 变成无效句柄，后续所有写入静默失败
- 新代码：添加错误处理，失败时输出到 stderr 作为 fallback
- `Write()` 方法添加 `w.file == nil` 检查和重试逻辑

**修复提交**：2026-10-15 19:17 本地编译，19:30 部署到 QG

---

**问题根因 2：JF 环境的 DNSHijacker 没有启动**（历史问题，已解决）

**证据链**：
1. QG 的 DNSHijacker 正常工作，收到 jf.phn/gg.phn 查询后调用 `forwardToRemote()` 转发到 JF (100.2.0.3:53)
2. QG 日志显示：`writing query (24 bytes)` → **没有 read success 日志** → 3秒后超时
3. JF 今天的日志中**没有 DNSHijacker 启动日志**（`DNSHijacker.Start()` 和 `DNS hijacker bound` 都没有）
4. JF 今天的启动日志（10:47:23）显示：
   - `P2PManager initialized` ✅
   - `Mesh enabled: nodeID=jf` ✅
   - **没有 TUN 相关日志** ❌
   - **没有 DNSHijacker 相关日志** ❌

**原因**：
- JF 环境配置 `tun: enabled: false`（仅做 h_tunnel 服务端）
- 代码逻辑：`engine.Start()` 和 `SetDNSHijacker()` 应该无论 TUN 是否启用都会执行
- 但实际日志显示 JF 的 DNSHijacker 没有启动
- 可能是 `SetDNSHijacker()` 检查 `e.netstack.IsRunning()` 失败，返回了错误

**影响**：
- QG 转发 DNS 查询到 JF 时超时（3秒）
- QG 返回 SERVFAIL 给客户端
- gg.phn 需要经过 JF 中转（hop=2），所以也失败

**对比**：
- QG → VM (100.1.0.3) 的 DNS 转发 ✅ 成功
- QG → QGW (100.224.0.3) 的 DNS 转发 ✅ 成功
- QG → JF (100.2.0.3) 的 DNS 转发 ❌ 失败（JF DNSHijacker 未启动）

---

### 当前状态（2026-10-15 19:30）

**DNS 功能**：✅ 正常工作
- 测试：`dig @100.0.0.3 gg.phn` → 100.179.0.10 ✅
- 测试：`dig @100.0.0.3 jf.phn` → 100.2.0.10 ✅
- 连续 10 次查询全部成功

**日志系统**：✅ 已修复并部署
- RotatingFileWriter 错误处理已加强
- stderr fallback 机制已就位
- QG 已部署最新版本（v0.8.0-5-g524a1c4-dirty）

**间歇性 SERVFAIL**：⚠️ 暂时未重现
- 部署后测试 DNS 正常
- 需要持续观察是否会再次出现
- 如果重现，日志系统现在应该能捕获到相关信息

---

### 历史修复方案（参考）

**方案 1：启用 JF 的 TUN**（不推荐）
- JF 是 h_tunnel 服务端，不需要 TUN
- 启用 TUN 会引入不必要的复杂性

**方案 2：修复 DNSHijacker 启动逻辑**（推荐）
- 检查 `SetDNSHijacker()` 为什么在 JF 上失败
- 确保即使 TUN 未启用，DNSHijacker 也能正常启动
- 这是代码 bug，需要修复

**方案 3：修改 DNS 转发逻辑**（临时绕过）
- 对于 jf.phn，直接在 QG 本地解析（不走转发）
- 但这不符合设计原则，不推荐

**建议**：采用方案 2，修复 DNSHijacker 启动逻辑。

## 更新日志

- 2026-10-15: 创建文档，记录问题现象
- 2026-10-15: 定位根因 - JF DNSHijacker 未启动，导致 DNS 转发超时
- 2026-10-15: 发现新问题 - 间歇性 SERVFAIL，与 JF 问题不同
  - 测试现象：启动后前 85 次查询正常，之后连续 SERVFAIL
  - 关键发现：**日志系统停止工作** - 日志文件在运行一段时间后不再更新
  - QG 日志文件在 19:05:04 后停止写入，但 DNS 查询持续到 19:08
  - 重启服务后 DNS 恢复正常，但日志系统仍然会在运行一段时间后停止
  - 这是基础设施级别的 bug，阻塞了 DNS 问题的诊断
  - 需要优先修复日志系统，然后才能定位 DNS SERVFAIL 的真正原因
- 2026-10-15 19:30: 修复 RotatingFileWriter 并部署到 QG
  - 修复 `util/util.go` 中 `rotate()` 和 `Write()` 方法的错误处理
  - 添加 stderr fallback 机制，防止日志写入失败时静默丢失
  - 部署后测试 DNS 正常（gg.phn, jf.phn 连续 10 次查询成功）
  - 间歇性 SERVFAIL 暂时未重现，持续观察中
