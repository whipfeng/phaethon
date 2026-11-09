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

- [ ] 池连接引用计数，使用中不回收
- [ ] 同目标 singleflight 建连
- [ ] 握手移出全局池锁

## 阶段 3: ctx 贯穿

- [ ] 各 dialer 实现 `DialContext`（trojan/vless/hysteria2/ssh/shadowsocks/htunnel/http/reverse/meshAware）
- [ ] 链内下一跳传递 ctx
- [ ] SSH channel open 可取消；htunnel/hysteria2 真实 deadline
- [ ] 健康检查使用 ctx 预算

## 阶段 4: P2P 版本协商（D25）

- [ ] 握手交换版本范围与能力位
- [ ] 新帧按能力启用
- [ ] 版本不满足保持周期探测

## 阶段 5: 健康检查配置/运行态分离（D14）

- [ ] 运行态表（组名+节点名 → 状态），不写回配置
- [ ] 每轮实时取组；DIRECT 真实探测；REJECT 排除
- [ ] 全局 ctx + 每节点预算；删除时清理运行态

## 阶段 6: 验证

- [ ] 三平台编译
- [ ] `go test ./dialer/... ./config/... ./db/...`（既有失败需区分）
