# deskzq v0.8.2 升级中断与离线（2026-11-18）

## 时间线

- 2026-11-17 22:03：通过 qg 控制台上传 v0.8.2 包并 POST publish；
  publish 响应前 SSH 连接被 reset（deskzq 的 mesh 连接断开）。
- 2026-11-18 10:47：复核发现 deskzq 离线：
  - `deskzq.phn` 被 qg 解析为本地 fakeIP `<IP_OTHER_006>`（非 deskzq 子网），控制台无响应；
  - jf 的 peers 只有 gg/qg/qgw，没有 deskzq；
  - qg 拓扑中没有 deskzq 的边；
  - 内置 SSH（mesh IP `<IP_DESKZQ_MESH_A>:22`）`Connection reset`。

## 影响

- 核心 9 节点中 8 个已是 `v0.8.2`：qg/gg/jf/mb/ms9/ms10/vm/qgw；
- deskzq 仍是上一版（v103）或升级中断后的未启动状态；
- 其余节点的 mesh、DNS、TUN 均正常。

## 恢复方式

deskzq 是 Windows 桌面节点，phaeton 需要在交互式桌面/计划任务中运行；
内置 SSH 与 phaeton 同进程，进程不在时没有远程入口。

需要在 deskzq 桌面/计划任务启动 `phaethon.exe`：
- 若 watchdog 已把 exe 换到 v0.8.2，启动后即为 v0.8.2；
- 若新 worker 启动失败，watchdog 会回滚 `.old`（v103），启动后先确认版本；
- 或提供 deskzq 的 OS SSH 入口，由 agent 继续完成升级与验证。

## 恢复（2026-11-18）

deskzq 已重新上线，无需再次发布：

- 控制台版本：`v0.8.2`（watchdog 已将中断前换上的 exe 拉起）；
- 拓扑：`jf ↔ deskzq` 双向边已恢复；
- 内置 SSH（`deskzq.phn:22`）正常；
- `/api/tun` 显示该节点 TUN 为关闭（`enabled=false`），属于该节点现有配置，未做变更。

至此 9 个节点全部为 `v0.8.2`。

## 备注

- 本次 publish 前 deskzq 的包已通过签名校验（id `78db5f34...`），不是包损坏；
- 其他 8 节点的 v0.8.2 已发布并验证。
