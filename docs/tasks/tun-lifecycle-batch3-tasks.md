# TUN 生命周期与清理批次（Batch 3）任务

> 创建日期: 2026-11-09
> 关联方案: [tun-lifecycle-batch3.md](../plans/tun-lifecycle-batch3.md)
> 关联审计: [full-code-audit-20261109.md](../issues/full-code-audit-20261109.md)
> 状态: 代码完成，待目标平台实机验证（2026-11-09）

## 阶段 1: 状态机与串行化（D8）— 完成

- [x] `Manager` 增加 `tunLifecycleState`（Idle/Starting/Running/Stopping）与 `lifecycleMu`
- [x] `StartTUN/StopTUN` 整个生命周期串行化，非法转换拒绝
- [x] `WriteLoopCloseCh` 改为仅 StopStack 关闭的稳定通道；TUN 停止时 writeLoop 不退出
- [x] 启动记录实际 DNS 接口名（`dnsIface`），停止用同一名字恢复
- [x] 启动失败调用 `routeMgr.TeardownForce()` 逆序回滚
- [x] `TeardownForce` 支持 Setup 未完成时的强制清理

## 阶段 2: Windows 清理（D19）— 完成

- [x] `removeAdapterAPI` 按 FriendlyName 严格匹配（不再“取第一个 WINTUN”）
- [x] `deleteResidualRoutesAPI(luid)` 按适配器 LUID 过滤
- [x] `StopTUN` 关闭设备后删除 `phaethontun` 适配器（`deleteTUNAdapter`）
- [x] TUN 禁用/不可用时也执行清理；物理口 DNS 仅在发现残留适配器时才重置
- [x] weak-host / DNS 原值持久化到 `data/state/windows-tun.json`，崩溃后恢复

## 阶段 3: Linux / macOS 清理（D20）— 完成

- [x] Linux 非 bypass 模式不再把 `rp_filter`/`ip_forward` 写 0
- [x] Linux DNS 停止使用启动时记录的接口名
- [x] Linux `resolvectl`/`nmcli` 原值持久化到 `data/state/linux-dns.json`，崩溃后恢复
- [x] Linux 路由加 `Protocol=200` 标记（拆分路由/排除路由/mesh 路由）
- [x] macOS DNS 原值持久化到 `data/state/darwin-dns.json`，崩溃后恢复
- [x] macOS `preemptForeignSplits` 快照落盘 `data/state/darwin-foreign-splits.json`，崩溃后恢复
- [x] iptables 维持现有“精确元组添加/循环删除”方式（评估后不做专用链：需重构 FORWARD 跳转与插入顺序，风险高、当前误删风险低）

## 阶段 4: 验证 — 完成（编译级）

- [x] Windows / Linux / Darwin 三平台编译通过
- [ ] 反复开关 TUN、kill -9 恢复（需目标平台实机验证）
- [ ] `go test ./tun/...`（既有 `NewEngine` 等历史失败，非本批引入）
