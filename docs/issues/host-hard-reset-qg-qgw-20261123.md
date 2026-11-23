# QG / QGW 同时宕机取证：宿主硬复位（2026-11-23 13:57）

## 现象

- 约 13:57–13:58，QG 与 qgw 同时失联；
- qgw 在 14:26:36 启动，QG 在 14:31:10 启动（QG 是 qgw 上的 VirtualBox
  VM `layer4-go`，宿主硬复位会同时带走 QG）；
- 此后 qgw 于 14:47 升级到 v47，15:00 之后无 warm lane 抖动。

## 时间线

| 时间 | 事件 |
|---|---|
| 13:36:57–13:37:06 | qgw watchdog 自升级 v38 → v45 成功，无异常 |
| 13:56–13:57 | qgw 日志仍有 1Hz warm lane churn（v45 已知缺陷：2 分钟内 78 次 warm add、254 次 remove） |
| 13:57:07 | QG 最后一条常规 SSH 探测会话断开（来自 `<IP_VM_OS>`） |
| 13:57:33 | qgw 记录的“上一次意外关机”时间（Event 6008） |
| 13:57:43 | qgw phaethon 日志最后一行，随后中断（无 panic/fatal，无收尾日志） |
| 13:58:00 | QG syslog 最后一条 cron 记录，随后整机失联 |
| 14:26:36 | qgw 宿主启动（WQPC） |
| 14:27:02 | qgw EventLog 服务启动（6005），6008 记录上次 13:57:33 关机为意外 |
| 14:30:45 | qgw 上 VirtualBox VM `layer4-go` 启动 |
| 14:31:10 | QG syslogd 启动，QG OS 恢复 |
| 14:34:24 | QG v47 pin `succeeded` |
| 14:47:34–14:47:43 | qgw v47 pin `succeeded` |

## 宿主侧证据（qgw / WQPC，Win7）

- Event 6008：上一次关机时间为 2026-11-23 13:57:33，标记为**意外**；
- Event 41（Kernel-Power）：`BugcheckCode=0`、`BugcheckParameter1..4=0x0`、
  `SleepInProgress=false`、`PowerButtonTimestamp=0`；
- 最近一次 Event 1074（主动关机/重启）为 2026-06-03；13:57 前后没有任何
  1074 / 6006 记录；
- 无 `C:\Windows\MEMORY.DMP`，无 minidump；Application 日志无 phaethon 的
  1000/1001/1026；仅有的 1001 是 `WindowsUpdateFailure3`（SafeLink.exe），无关；
- phaethon 日志最后一行 13:57:43 后直接截断，进程没有机会写退出/panic 信息。

结论：宿主是**瞬间断电 / 硬复位**型重启，不是蓝屏（无 bugcheck、无 dump），
也不是主动关机。

## 同日的第二个宿主事件（VM 环境宿主）

- 本机宿主 `WIN-9UF9F42UQFU`：Event 6008 记录上一次关机时间为
  2026-11-23 06:52:46 为意外，10:58:06 启动；
- 对应 Event 41 同样是 `BugcheckCode=0`、`PowerButtonTimestamp=0`；
- 与用户反馈“VM 环境宿主之前也宕过”一致；VM 节点在宿主恢复后才恢复。

同日两台独立 Windows 宿主、相同签名（BugcheckCode=0、无 1074、无 dump）
→ 外部供电 / 硬复位类问题的概率远高于应用缺陷。

## 与 phaethon 的关系

- 崩溃前 20 分钟 qgw 刚完成 v45 自升级且成功（13:37:06 supervision transferred），
  之后持续正常运行；
- phaethon 无 panic / OOM / WER，日志被宿主硬切断；
- qgw 当时仍运行 v45，日志显示已知的 1Hz warm lane churn
  （见 `p2p-warm-lane-churn-20261123.md`）。该缺陷带来频繁 WS 建连/拆除负载，
  但没有任何内核转储或资源耗尽事件能证明它导致宿主复位；
- 复位后 qgw 升级 v47：15:00 后 warm add/remove 均为 0，lanes 稳定 2 条，
  `[STRIPE] diag` 正常。

## 当前状态

- 拓扑 API（QG 侧）：9 节点、11 对双向链路（22 条有向边）；
- QG：`v0.8.3-47-g610339f`，pin `succeeded`（14:34:24）；
- qgw：`v0.8.3-47-g610339f`，pin `succeeded`（14:47:43），无 warm lane churn。

## 建议

- 排查两台宿主的供电 / PDU / UPS 与机房环境（同日不同宿主、BugcheckCode=0
  的硬复位更像外部供电事件）；
- 应用侧无法防御瞬时断电（进程没有退出机会），现有 watchdog + 开机自启动
  已能在宿主恢复后自动拉起；
- 后续再发生，优先看宿主事件日志（6008/41/1074）、dump 与供电记录，
  而不是先怀疑应用。

## 经验

- 多个节点同时失联，先画“节点 → 宿主 / VM”关系图：同一宿主上的 VM 会一起消失；
- Event 41 `BugcheckCode=0` + 无 1074 + 无 dump = 外部硬复位 / 掉电，
  不是应用 panic；
- 应用日志“最后一行直接截断”是宿主级断电的典型特征。
