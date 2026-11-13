# Watchdog 自替换升级设计

> 状态：已实现、VM/QG 验证通过，并已扩散到全部节点（2026-11-13，含 ms9）
> 关联：`64fe6d6`（停用版本化 worker / 稳定路径）、`upgrade_apply.go`、`pkg/upgrade`
> 验证记录：`docs/issues/mesh-transfer-throughput-20261109.md` §139

## 1. 目标

升级不再依赖“新文件名 worker”，而是**始终替换稳定路径上的同一个可执行文件**并重启：

- Windows 防火墙 / AV / EDR 的程序规则按完整路径匹配，路径不变规则才继续生效；
- 计划任务、服务注册、开机自启都指向同一路径；
- 升级能力仍由管理员显式触发，不做“扫到新包就自动跳版”。

## 2. 触发方式（显式 activate + 请求文件）

```
上传 pkg → data/packages-pending/          （不生效）
     │
     │ 管理员在控制台点“启用”（可逐节点/逐批）
     ▼
data/packages/<id>.pkg                     （已发布，可分发）
data/state/upgrade-request  {version,...}  （仅平台/架构匹配本机时写入）
     │
     │ watchdog 每 2s 轮询
     ▼
自替换流程
```

不采用定期扫描 `data/packages` 自动升级：误传包会导致全网自动跳版，且无法分批。

## 3. 自替换流程（watchdog 执行）

```
upgrade-request {vX}
 ├─ 在 data/packages 找到该版本：验签 + platform/arch 匹配
 ├─ 解压到 <exe>.new（暂存，不改变运行中的进程）
 ├─ 写 upgrade-in-progress marker
 ├─ 停 worker（SIGTERM → 10s → SIGKILL）
 ├─ <exe> → <exe>.old
 ├─ <exe>.new → <exe>          ★ 原路径
 └─ 启动 <exe> 作为 worker
      ├─ 20s 内 ready 且上报 version == 请求版本
      │    → status=succeeded，记录 worker-version，清 request/marker
      └─ ready 超时 / 提前退出 / 版本不符
           → <exe> → <exe>.failed
           → <exe>.old → <exe>
           → 启动回旧版
           → status=failed-rolled-back，清 request（不循环重试）
```

要点：

- **验签与暂存都在停 worker 之前完成**，包缺失或损坏不会中断服务。
- 包只从 `data/packages`（已启用）读取；pending 包永不执行。
- 备份保留 `.old` 与 `.old2` 两版；回滚使用 `.old`，坏二进制保留为 `.failed` 便于取证。
- 请求文件已生效（运行版本 == 请求版本）时幂等标记成功，避免 watchdog 重启后重复替换。

## 4. 断电 / 进程被杀的安全网

升级前写 `data/state/upgrade-in-progress {old,new,version}`，watchdog 启动时先修复：

| 启动时状态 | 处理 |
| --- | --- |
| `<exe>` 存在 | 认为交换已完成；清理残留 `.new`；若 marker 存在则继续等 ready 校验 |
| `<exe>` 缺失、`.old` 存在 | 恢复 `.old` → `<exe>`，清 request/marker，status=failed-rolled-back |
| `<exe>` 缺失、无 `.old`、`.new` 存在 | 安装 `.new` → `<exe>`，继续 ready 校验 |

残余风险：Windows 计划任务直接执行 `<exe>`，若断电恰好发生在两个 rename 之间（微秒级窗口），
开机时启动器找不到文件，watchdog 无从运行。部署侧可在启动脚本（VM 的 vbs 启动器）加入
“缺 `<exe>` 时先恢复 `.old`” 的同款修复；代码内已覆盖 watchdog 能启动的所有场景。

## 5. supervisor 与 worker 版本

- worker（数据面）：activate 后数秒内变成新版本，路径不变。
- supervisor（watchdog）：内存里仍是旧版本，磁盘文件已是新版本；下次服务/系统重启时自然变成新版本。
- supervisor 只负责拉起与监控，不承载数据面，因此延迟更新可接受，也避免自我 exec 的重启风暴。

## 6. 状态与可观测性

`GET /api/packages/pin`（沿用旧路径）返回：

```json
{
  "current": "v0.8.0-664-gxxxx",
  "requested": "v0.8.0-665-gyyyy",
  "status": "idle|applying|succeeded|failed-rolled-back",
  "lastError": "",
  "updatedAt": "..."
}
```

- `DELETE /api/packages/pin` 取消尚未开始的请求；`applying` 阶段返回 409。
- 控制台 Package 页横幅展示：等待应用 / 应用中 / 成功 / 失败已回滚（含原因）。
- 旧 `data/state/worker-pin` 不再被读取，启动/启用/删除包时自动清理。

## 7. 安全约束

- 只应用**签名通过**且 platform/arch 匹配的包；未验签的包不会换二进制。
- 不新增网络监听、不引入提权；watchdog 只需部署目录写权限。
- Windows 允许重命名运行中的 exe；稳定路径 `phaethon.exe` 建议加入杀软白名单一次。
- 跨平台包在控制台“启用”时只做分发，不写入本机升级请求（`localUpgrade=false`）。

## 8. 验证计划

1. 单测：请求/状态读写、swap 顺序、回滚、启动修复、签名包查找、已运行幂等、缺包不伤服务。
2. VM（Windows）：手动发一版 → 上传下一版 pkg → 控制台启用 → 观察 watchdog 日志、进程路径仍为
   `phaethon.exe`、版本变为目标版本、防火墙规则仍生效。
3. QG（Linux）：同上，验证路径不变、OpenRC 服务仍可管理。
4. 故障注入：坏包 / ready 失败 → 自动回滚；升级中杀 watchdog → 启动修复。
5. 两项通过后再扩散到 jf/gg/mb/qgw/deskzq/ms。
