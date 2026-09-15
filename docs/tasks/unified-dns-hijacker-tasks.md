# 统一 DNS hijacker 方案

## 任务信息

- 分支名：`unified-dns-hijacker`
- 目标：删除 tryDNSRedirect 拦截机制，由 DNS hijacker 统一处理 Mode A/B 所有 DNS 查询，支持跨节点转发和缓存
- 创建日期：2026-09-15
- 依赖计划：[mesh_network_improvements.md](../plans/mesh_network_improvements.md) v0.8.0 §3.8

## 阶段 1: 回退拦截代码

参见设计方案 §3.8.7（15 处）。

### Task 1.1: tun/engine.go — 删除 meshGatewayResolver 相关
- [x] 删除 `meshGatewayResolver` 字段（line 115）
- [x] 删除 `SetMeshGatewayResolver()` 方法（lines 150-154）
- [x] 删除 `InjectMeshPacket` 中的 `tryDNSRedirect` 调用（lines 351-357）
- [x] 删除 readLoop 中的 DNS debug 日志（lines 1076-1093）
- [x] 删除 readLoop 中的 `tryDNSRedirect` 调用（lines 1095-1101）
- [x] 删除 `tryDNSRedirect` 函数本身（lines 1127-1201）
- [x] 删除 writeLoop 中的 `tryDNSRedirect` 调用（lines 1305-1313）
- [x] 更新 `queryInternalDNS` 注释（lines 1776-1777），去掉 tryDNSRedirect 引用

### Task 1.2: mesh/mesh.go — 删除网关解析和 VIP 路径 DNS 处理
- [x] 删除 `GetGatewayGIPForDomain()` 方法（lines 403-432）
- [x] 删除 `ResolveGatewayGIP()` 方法（lines 434-438）
- [x] 删除 VIP 路径 DNS 响应特殊处理：isDNS 检测 + `TranslateInboundWithSrc` 分支（lines 587-607），简化为统一 `TranslateInbound`
- [x] 删除 VIP NAT reverse 失败时的 fallback 代码（lines 621-659），注释引用 tryDNSRedirect
- [x] 更新 VIP 路径相关日志格式

### Task 1.3: main_tun.go — 删除 SetMeshGatewayResolver 调用
- [x] 删除 `engine.SetMeshGatewayResolver(meshMgr.ResolveGatewayGIP)` 调用（line 105）

### Task 1.4: 编译验证
- [x] `go build ./...` 通过（Linux + Windows 交叉编译）
- [x] 无未使用的 import（go vet 通过）

## 阶段 2: P2P 协议版本升级 + Gossip 格式扩展

### Task 2.1: 升级 P2P 协议版本
- [x] `p2p/p2p.go`：`P2PProtocolVersion = 1` → `2`

### Task 2.2: 扩展 Gossip 格式
- [x] `mesh/topology.go`：`GossipDomainSuffix` 增加 `Subnet string` 字段
- [x] `mesh/topology.go`：`PeerDomainSuffixEntry` 增加 `Subnet *net.IPNet` 和 `SubnetStr string` 字段
- [x] 更新 gossip 序列化/反序列化逻辑，传递 Subnet 信息

### Task 2.3: Mesh 层传播 Subnet
- [x] `mesh/mesh.go`：gossip 处理时解析 Subnet 字段，存入 `PeerDomainSuffixEntry`
- [x] 提供方法供 DNS hijacker 查询：`ResolveDomainSubnet(domain) → *net.IPNet`
- [x] `mesh/domain_trie.go`：trie node 增加 subnet 字段，Insert/Lookup 签名更新

### Task 2.4: 编译验证
- [x] `go build` 通过（Linux + Windows 交叉编译）
- [x] `go test ./mesh/...` 通过
- [x] `go vet` 通过

## 阶段 3: DNS hijacker 跨节点转发

### Task 3.1: DNSHijacker 结构扩展
- [x] `tun/dns.go`：新增 `resolveDomainSubnet` 回调（从 mesh 层查询域名归属）
- [x] 新增 `cache` 字段（DNSCache 实例）
- [x] 新增 `SetDomainResolver()` 方法
- [x] `tun/engine.go`：新增 `SetDNSDomainResolver()` 方法委托给 DNS hijacker

### Task 3.2: DNS 缓存
- [x] `tun/dns.go`：实现 `DNSCache` 结构（domain → Fake-IP + expiry）
- [x] 缓存命中时直接返回，不转发
- [x] 缓存未命中时转发到远端，收到响应后写入缓存（TTL 5 分钟）

### Task 3.3: serveLoop 改造
- [x] 收到 DNS 查询后，先检查缓存
- [x] 调用 `resolveDomainSubnet` 判断域名归属
- [x] 远端域名：通过 `gonet.DialUDP` 转发到远端 GIP:53
- [x] 本地域名或无匹配：从本地 Fake-IP 池分配（现有逻辑）
- [x] `main_tun.go`：wire up `engine.SetDNSDomainResolver(meshMgr.ResolveDomainSubnet)`

### Task 3.4: 编译验证
- [x] `go build` 通过（Linux + Windows 交叉编译）
- [x] `go vet` 通过
- [x] `go test ./mesh/...` 通过

## 阶段 4: 部署验证

需同时部署所有节点（QG、VM、JF），因为 P2P 协议版本升级。

### Task 4.1: 编译
- [ ] `make linux` + `make windows`
- [ ] 上传到各环境

### Task 4.2: 验证 Mode A DNS
- [ ] VM 上 nslookup qg.phn → 返回 Fake-IP
- [ ] VM 上 nslookup vm.phn → 返回 Fake-IP（本地）
- [ ] QG 上 nslookup vm.phn → 返回 Fake-IP

### Task 4.3: 验证 Mode B DNS
- [ ] 通过 QG SOCKS5 代理访问 VM 域名服务 → 正常
- [ ] 通过 VM SOCKS5 代理访问 QG 域名服务 → 正常

### Task 4.4: 验证 DNS 缓存
- [ ] 首次查询远端域名 → 转发到远端 hijacker
- [ ] 再次查询同一域名 → 缓存命中，不转发

### Task 4.5: 验证 P2P 版本兼容性
- [ ] 旧版本（v1）连接新版本（v2）→ 被拒绝
