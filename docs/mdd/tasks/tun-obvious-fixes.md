# TUN 显而易见问题修复

## 任务信息

- 分支名：`master`
- 目标：修复 TUN 代码 review 中发现的 3 个明显问题
- 依赖计划：[tun_obvious_fixes.md](../plans/tun_obvious_fixes.md)
- 注意事项：**不在当前工作主机上启用真实 TUN**，避免网络中断

## 阶段 1: readLoop 修复

### Task 1.1: IPv4/IPv6 协议识别
- [x] `readLoop` 根据 IP 版本字段选择 `ipv4.ProtocolNumber` 或 `ipv6.ProtocolNumber`
- [x] 非 IP 包丢弃

### Task 1.2: 每包独立 buffer
- [x] 不再复用单个 `buf := make([]byte, 2048)`
- [x] 每包复制到独立 slice 后注入 netstack

## 阶段 2: proxyDesc 大小写修复

### Task 2.1: DIRECT 判断
- [x] 使用 `strings.EqualFold` 比较 `proxy.Type` 与 `config.ProxyDIRECT`

## 阶段 3: LAN/私网排除

### Task 3.1: 默认 LAN CIDR 排除
- [x] 新增 `DefaultLANExclusions`：10/8、172.16/12、192.168/16、127/8、169.254/16、224/4、255.255.255.255/32
- [x] `Start()` 将 LAN CIDR 与代理服务器 IP 一并传给 `RouteManager.SetExclusions`

### Task 3.2: RouteManager 支持 CIDR 排除
- [x] Windows/Linux/Darwin 的 `platformSetup` 同时处理纯 IP 与 CIDR
- [x] `deleteExclusionRoute` 根据字符串是否含 `/` 区分主机路由与网络路由

### Task 3.3: 级联代理只排除第一跳
- [x] 新增 `firstProxyHop()` 沿 `proxy.Next` 找到真正要从本机连出去的那个代理
- [x] `resolveProxyIPs()` 只解析并排除第一跳的服务器，中间/末端代理通过隧道可达，不需要排除
- [x] 增加 `TestFirstProxyHop` 单元测试覆盖链式、DIRECT next、环状配置

## 阶段 4: UDP 报文转发

### Task 4.1: 使用 PacketConn  preserves datagram semantics
- [x] `handleUDP` 改为调用 `dialer.ChainUDPDial` 获取 `net.PacketConn`
- [x] DIRECT UDP 走新接口 `directDialPacket()`，绑定原物理网卡
- [x] 新增 `relayUDP()` 逐包转发，保持 UDP 数据报边界
- [x] 移除原先把 UDP 当 TCP 流的 `ChainDialWithID` + `util.Relay` 写法

## 阶段 5: 验证

### Task 5.1: 单元测试
- [x] `go test ./tun -v`
- [x] `go test ./...`

### Task 5.2: 构建
- [x] `go build ./...`
- [x] `go vet ./tun`

## 阶段 6: 提交

### Task 6.1: 正常提交
- [x] `git add` 相关文件
- [x] `git commit` 新提交（不 amend）
- [ ] 推送（用户指示不着急 push，待后续网络恢复再推）
