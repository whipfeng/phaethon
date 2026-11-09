# 健康检查与拨号批次（Batch 4）任务

> 创建日期: 2026-11-09
> 关联方案: [health-dial-batch4.md](../plans/health-dial-batch4.md)
> 关联审计: [full-code-audit-20261109.md](../issues/full-code-audit-20261109.md)
> 状态: 进行中

## 阶段 1: 订阅并发一致性（D16）

- [x] 所有 `ActiveMember` 直接赋值改为 `SetActiveMember`（admin 两处 handler + main.go 刷新路径）
- [x] 网络失败 DB 回退分支与成功分支共用组重建逻辑
- 注：`SetActiveMember` 对「无 resolver 回调的 DB 副本」跳过成员校验（否则订阅节点名一律被判 not found，
  管理接口不可用）；有 resolver 的运行态组仍严格校验

## 阶段 2: SSH 连接池（D24）

- [x] 池连接引用计数，使用中不回收（`getSSHClient` 返回即持有一份引用，转发通道 Close 时归还）
- [x] 同目标 singleflight 建连（`sshKeyLocks`，重连走 `reconnMu`）
- [x] 握手移出全局池锁（`sshCacheMu` 只保护 map 与引用计数）

## 阶段 3: ctx 贯穿

- [x] 各 dialer 实现 `DialContext`（trojan/vless/hysteria2/ssh/shadowsocks/htunnel/http/direct/socks5）
- [x] 链内下一跳传递 ctx（`dialHandshakeConn` / `dialWithContext`，含 htunnel 的 HTTP Transport DialContext）
- [x] SSH channel open 可取消（`openConnCtx` 包装，取消后迟到通道自动关闭）；htunnel 每个请求已有 ctx 超时
- [x] 健康检查使用 ctx 预算（`healthCheckBudget` + `ChainDialContext` / `DialToProxyContext`）
- 注：`DialReverse` / 控制连接不在拨号链上，未纳入 ctx 改造

## 阶段 4: P2P 版本协商（D25）

- [x] 握手交换版本范围与能力位（gossip 携带 `MinProtocolVersion` + `Capabilities`，对端取交集记录）
- [x] 新帧/新特性按对端能力启用（动态 lane 需协商版本 ≥10 且对端声明 `stripe` 能力，否则拒绝 attach/dial）
- [x] 版本不满足保持周期探测（只断开本次连接，`StartPeer` 继续重连）
- 注：对端未声明能力（旧版本构建）按「不限制」处理，避免升级窗口内误禁用；协商结果进入 `/api/p2p` 状态

## 阶段 5: 健康检查配置/运行态分离（D14）

- [x] 每轮实时取组（1s tick 重读 matcher，按各组 interval 到期才检查；删除的组自动停止检查）
- [x] 健康结果不再写回组配置（移除 `db.PutProxyGroup`），消除「已删除组被复活」「healthMap 被覆盖」
- [x] DIRECT 在配置了检查 URL 时走真实探测；REJECT 标记不可用、不参与 best/load-balance 选择
- [x] 全局 ctx + 每节点预算（`healthCheckBudget`）；删除的组不会再被检查，运行态随之释放
- [x] 运行态表外置：`config.healthRegistry`（组名 → 成员 key → 状态）进程级持有，组对象不再存健康数据；
  组对象被配置重载替换时健康状态不丢，`DeleteProxyGroup` 时清理对应 bucket

## 阶段 6: 验证

- [x] 三平台编译（Windows 本机 + GOOS=linux/darwin）
- [x] `go test ./config/... ./db/... ./p2p/... ./server/... ./admin/...` 通过；`go test -vet=off ./mesh/...` 通过
- 注：`go test ./dialer/...` / `./reverse/...` 已可编译通过。两个历史 e2e 文件
  （`reverse/e2e_test.go`、`dialer/reverse_udp_e2e_test.go`）依赖早已删除的 `config.RuleConfiguration`，
  已用 `//go:build ignore` 挂起待移植，见 docs/runbooks/verification-checklist-20261109.md 的「已知遗留」

## 收尾说明

批次 4 已完成：D16 订阅并发、D24 SSH 池、ctx 贯穿、D25 版本协商、D14 健康检查（除“运行态表彻底外置”）。
后续若要把健康运行态从组对象剥离，需要同时改 `config.ProxyGroup` 的选择逻辑与 admin 健康接口，建议单独立项。
