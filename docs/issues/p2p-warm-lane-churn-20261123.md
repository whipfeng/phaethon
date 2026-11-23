# P2P warm lane 每秒重建抖动（2026-11-23）

## 现象

- QG / JF 的 p2p 日志出现约 **1Hz** 的成对事件：
  `[STRIPE] warm lane stripeID=...` 与 `[STRIPE] remove lane stripeID=...`；
- QG 24 小时窗口内 `remove lane` **1159 条**，JF 旧版本（v0.8.3-45）日志
  累计 **4209 条**；
- 每次 remove/attach 都会 `dropAffinity()` 全量清空流亲和并重建 WS carrier，
  表现为连接周期性抖动、RTT 尖峰、偶发回退长轮询；
- 之前 11-23 上午观测到的 “warm2 keeper 秒级补齐、稳态振荡 2↔1↔2” 曾被
  误判为设计行为，实际就是这个缺陷循环（见 `elephant-dashboard-slow-20261123.md`
  的结论修正）。

## 现场证据

| 环境 | 版本 | 观测 |
|---|---|---|
| QG | v0.8.3-45 | 24h `remove lane` 1159 条，间隔约 1s，与 warm keeper 的 add 严格配对 |
| JF | v0.8.3-45 | 旧日志累计 `remove lane` 4209 条 |
| QG | v0.8.3-47-g610339f | 重启后 8 次 warm lane 建连、仅 3 次 remove；3 次均对应对端会话结束（jf EOF、mb `connection reset by peer`），随即为新会话补 warm lane；无 idle 老化 remove、无 1Hz 循环 |
| 机群 | v0.8.3-47-g610339f | 9 节点 pin `status=succeeded`（QG 14:34、qgw 14:47、mb 14:47）；拓扑 9 节点、22 条双向边 |

## 根因

`p2p/stripe_transport.go` 中四个缺陷相互叠加，任一个都足以让 lane 集合
无法稳定：

1. **空集合也会 arm `clearSince`**：`controllerTick` 在 lane 集合为空时把
   `clearSince` 置为当前时间，下一次 tick 立即满足
   “存在 lane + idle ≥ `stripeClearSustain`” 的 `removeDue` 条件。
2. **新增 lane 未重置空闲计时**：`attachLane` 成功后没有清空 `clearSince`，
   刚 attach 的 lane 在下一 tick 就被判定为可回收。
3. **warm lane 被 idle 老化误伤**：controller 的 idle victim 选择直接取
   “最新的 lane”，把 warm keeper 主动预建的 lane 也算了进去；移除后 keeper
   立即重拨，形成 1Hz 重连 + affinity 反复失效。
4. **`removeLane` 非幂等**：controller 与 lane 自身的 writer/reader goroutine
   会竞争同一条 lane，第二次调用仍会打印日志并重复 rehash/close。

## 修复（commit 610339f）

- `stripeLane` 增加 `warm` 标记，新增 `attachLaneKind`，warm keeper 以
  `warm=true` 挂载 lane；
- warm lane 不参与 idle 老化，victim 选择跳过 warm lane；
- `attachLane` 成功后重置 `clearSince`；非 warm lane 数为 0 时不再累积
  `clearSince`（`nonWarmLaneCount`）；
- `removeLane` 幂等：未找到目标 lane 直接返回，不重复日志/rehash/close；
- 动态 lane 的 idle 老化保留，仅排除刻意预建的 warm lane，不改变既有
  扩缩容语义与保序保证。

## 测试

新增并通过：

- `TestStripeAttachLaneResetsIdleTimer`
- `TestStripeWarmLaneSurvivesIdleAgeOut`
- `TestStripeRemoveLaneIdempotent`

`go test ./p2p` 全绿，`go vet ./p2p` 无告警，`go build ./...` 通过。

## 部署

- 版本 `v0.8.3-47-g610339f`，签名包：
  `dist/linux-amd64`、`dist/darwin-amd64`、`dist/windows-amd64`、
  `dist/windows7-amd64`（qgw 为 Win7，需独立平台包）；
- 先 QG/JF 验证无 1Hz 循环，再全量扩散；9 节点全部 `status=succeeded`。

## 相关事件

- 2026-11-23 13:57 qgw 宿主意外硬复位，qg（qgw 上的 VirtualBox VM）随之中断；
  已排除 phaethon 崩溃，取证见 `host-hard-reset-qg-qgw-20261123.md`。

## 经验

- lane 生命周期事件必须成对可解释：出现稳定频率的 add/remove 配对时，优先
  怀疑“移除条件把刻意保活的载体也算进去”，而不是调大阈值；
- warm/pre-created 资源要与“按需资源”在数据结构上区分（显式标记），否则
  任何通用回收逻辑都会误伤；
- 幂等是并发移除路径的硬要求，否则日志与 rehash 会被放大，掩盖真实频率。
