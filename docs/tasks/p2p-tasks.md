# P2P 节点互联与自动分发

## 任务信息

- 分支名：`p2p-distribution`
- 目标：实现 phaethon 实例间自动建立 P2P 连接，交换版本信息，低版本自动从高版本拉取二进制并自更新
- 创建日期：2026-09-08
- 依赖计划：[p2p_distribution_design.md](../plans/p2p_distribution_design.md)

## 阶段 1: 基础通道

### Task 1.1: 新增常量
- [x] `reverse/control.go` 新增 `BindPortP2P = 2`

### Task 1.2: 删除 ResolveCmd
- [x] `dialer/dialer.go` 删除 `ResolveCmd()`、`IsBind()`、`CmdType` 字段
- [x] `dialer/socks5.go` `Dial()` 中 `ResolveCmd(dstPort)` 改为 `byte(0x01)`
- [x] `dialer/trojan.go` `Dial()` 中 `ResolveCmd(dstPort)` 改为 `byte(0x01)`
- [x] `dialer/htunnel.go` `Dial()` 删除 `IsBind` 分支，始终用 `"CONN"`
- [x] `dialer/htunnel.go` 重写 `DialControl()` — 不再委托给 `Dial()`，自己实现 HTTP tunnel + 硬编码 BIND PORT=1
- [x] `dialer/base_test.go` 删除 ResolveCmd/IsBind 相关测试
- [x] `dialer/htunnel_test.go` 删除 CmdType 相关测试

### Task 1.3: Server 端 PORT=2 路由
- [x] `server/socks5.go` BIND 处理段增加 `dstPort == BindPortP2P` 分支 → `handleP2PConnection()`
- [x] `server/trojan.go` BIND 处理段增加 `dstPort == BindPortP2P` 分支 → `handleP2PConnection()`
- [x] `server/htunnel.go` `handleConnectionPush` 增加 `port == BindPortP2P` 分支 → `handleP2PConnection()`

### Task 1.4: Dialer 端 DialP2P
- [x] `dialer/socks5.go` 新增 `DialP2P()` — ChainDial + 硬编码 BIND PORT=2
- [x] `dialer/trojan.go` 新增 `DialP2P()` — nextDialer.Dial + TLS + 硬编码 Trojan BIND PORT=2
- [x] `dialer/htunnel.go` 新增 `DialP2P()` — HTTP tunnel + 硬编码 `X-C: BIND, X-P: "2"`

### Task 1.5: 编译验证
- [x] `go build ./...` 通过
- [x] `go test ./dialer/...` 通过

## 阶段 2: P2P 模块骨架

### Task 2.1: P2PManager 结构
- [x] 新建 `p2p/p2p.go`
- [x] `P2PManager` 结构体（peers map、nodeId、version、platform、arch）
- [x] `Peer` 结构体（ID、NodeID、Version、Platform、Arch、Checksum、Status、LastSeen）
- [x] `NewP2PManager(nodeId, version, platform, arch)` 构造函数

### Task 2.2: handleP2PConn — 服务端接受连接
- [x] `HandleP2PConn(conn net.Conn)` 方法
- [x] 包装为 `FramedConn`（复用 `reverse/frame.go`）
- [x] 启动心跳循环（FrameHeartbeat，10s 间隔，60s 超时）
- [x] 接收帧循环：FrameData → JSON 解析 → 命令分发

### Task 2.3: StartPeer — 客户端发起连接
- [x] `StartPeer(proxy *config.Proxy)` 方法
- [x] 根据 proxy.Type 调用对应 `DialP2P()`
- [x] 连接成功后包装为 `FramedConn`，启动心跳 + 接收循环
- [x] 失败静默跳过，打印 debug 日志

### Task 2.4: 编译验证
- [x] `go build ./...` 通过

## 阶段 3: 版本交换

### Task 3.1: hello 命令
- [x] 连接建立后双方各发一个 `hello` 命令（FrameData JSON）
- [x] `handleHello(peer, msg)` — 记录 peer 信息，版本比对
- [x] 版本相同 → 标记 `upToDate`
- [x] peer 版本更高 → 客户端发起 `update_request`
- [x] 自身版本更高 → 等待 peer 发起 `update_request`

### Task 3.2: Admin API
- [x] `admin/admin.go` 新增 `GET /api/p2p` — 返回 peer 列表及状态
- [x] `main.go` 启动时遍历 proxies，对 socks5/trojan/h_tunnel 类型调用 `StartPeer()`

### Task 3.3: 编译 + 部署验证
- [x] `go build ./...` 通过

## 阶段 4: 二进制传输

### Task 4.1: update_request + manifest
- [x] `update_request` 命令处理 — 高版本读取自身二进制，计算分片 SHA-256
- [x] `manifest` 命令发送 — chunkSize=512KB，chunkHashes 列表

### Task 4.2: chunk_req + chunk_data
- [x] `chunk_req` 命令处理 — 按 chunkIndex 读取对应分片
- [x] `chunk_data` 发送 — 先送 JSON 头帧（chunkIndex + frameSeq + totalFrames），后续帧纯二进制
- [x] 接收方逐帧写入临时文件，收完一个 chunk 后校验 SHA-256

### Task 4.3: update_ack
- [x] 全部 chunk 校验通过 → 发送 `update_ack{status:"ok"}`
- [x] 任一 chunk 校验失败 → 发送 `update_ack{status:"chunk_mismatch"}`，删除临时文件

## 阶段 5: 自更新

### Task 5.1: 备份 + 替换 + 退出
- [x] `applyUpdate()` — 备份当前二进制 → phaethon.bak
- [x] Linux: `rename("phaethon.new", "phaethon")`
- [x] Windows: `rename("phaethon.exe", "phaethon.bak")` → `rename("phaethon.new", "phaethon.exe")`
- [x] 日志记录后 `os.Exit(0)`，watchdog 拉起新版本

## 阶段 6: 健壮性

### Task 6.1: 重连与容错
- [x] P2P 连接断开后自动重连（间隔递增）
- [x] platform/arch 不匹配时标记 `unsupported`，不触发更新
- [x] 传输中断后清理临时文件，等待下次触发

## 阶段 7: 收尾

### Task 7.1: 最终验证
- [ ] `go test ./...` 无回归
- [ ] `make windows` + `make linux` 编译通过
- [ ] 部署到 VM 环境，验证完整更新流程
- [ ] 更新 `docs/index.md`
- [ ] 标记所有 task 为 [x]
- [ ] commit 并 push
