# 部署环境运维手册

> 版本: 0.1.0
> 日期: 2026-09-10
> 状态: ACTIVE
> 负责人: Phaethon Dev

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-09-10 | 初始版本 | Qoder |

> **注意：本文档将提交到 Git 仓库，禁止存放任何敏感信息。**
> 包括但不限于：IP 地址、SSH 端口/凭据、密码、Token、API Key、内网域名等。
> 敏感信息仅存放在 `agents.md`（不入库，仅作为本地 agent 上下文加载）。

## 1. 环境角色

| 环境 | 系统 | 角色 | 进程管理 |
|------|------|------|----------|
| QG | Alpine Linux + OpenRC | 主服务/生产 | `rc-service phaethon` 守护进程，自动拉起 |
| VM | Windows 10 | 开发/测试 | 计划任务 + VBS 脚本，交互式桌面会话 |
| JF | CentOS/RHEL | H_Tunnel 服务端 | 手动 nohup，无守护进程 |

## 2. VM 启动方式（重要）

### 必须通过计划任务启动

VM 环境的 phaethon **必须通过 Windows 计划任务 `PhaethonTUN` 启动**，不能通过 SSH 直接 `Start-Process`。

**启动链路**:

```
计划任务 PhaethonTUN
  → wscript.exe start-phaethon.vbs
    → phaethon.exe（隐藏窗口，交互式桌面会话）
```

**VBS 脚本** (`start-phaethon.vbs`):

```vbs
Set WshShell = CreateObject("WScript.Shell")
WshShell.Run "phaethon.exe", 0, False
' 0 = 隐藏窗口, False = 异步（不等待）
```

### 为什么不能 SSH 直接启动

phaethon 需要在用户的**交互式桌面会话**中运行：

- **Wintun 驱动**依赖桌面会话上下文
- **UAC 提权**（ShellExecute "runas"）需要桌面会话
- SSH 启动的进程运行在非交互式会话中，约 2 秒后 hard crash（无 Go panic 输出，疑似 access violation）

### 停止

```powershell
Stop-Process -Name phaethon -Force
```

会杀掉所有 `phaethon.exe` 进程（watchdog + worker）。

## 3. 进程架构

### 看门狗已合并到主程序

`phaethon.exe` 启动后自动产生 2 个进程：

| 进程 | 环境变量 | 职责 |
|------|----------|------|
| watchdog（父进程） | 无 `PHAETHON_WORKER` | 监控 worker，crash 后自动重启 |
| worker（子进程） | `PHAETHON_WORKER=1` | 实际业务逻辑（代理、TUN、mesh 等） |

不再有独立的 `phaethon-watchdog.exe` 二进制。

### QG 环境

OpenRC supervise-daemon 管理，进程挂掉自动拉起，开机自启。

### JF 环境

手动 `nohup` 启动，无自动拉起。进程挂掉需要手动重启。

## 4. 部署注意事项

### 通用

- 部署新二进制前先停止服务
- 部署后检查日志确认启动成功
- QG 和 VM 不能同时关闭，始终保持至少一个环境可用

### Windows (VM)

- SCP 上传 exe 时如果文件正在运行，可能静默失败（文件被锁定）
- 解决：先 Stop-Process → 上传 → Start-ScheduledTask
- 残留 Wintun 设备排查：`pnputil /enum-devices /class Net`
- 清理残留：`pnputil /remove-device <instance-id>`

### Linux (QG/JF)

- QG 用 `rc-service` 管理，不要直接 kill
- JF 用 `ps | kill` 手动管理

## 5. 升级与 worker 版本（务必阅读）

自 2026-11-13 起统一为“稳定路径自替换”，详细设计见
[watchdog_self_replace_upgrade.md](../plans/watchdog_self_replace_upgrade.md)。

### 5.1 正常升级流程

1. 控制台上传 `.pkg` → 进入 `data/packages-pending/`（不生效）；
2. 管理员点“启用”（可逐节点/逐批）→ 包移到 `data/packages/`；
   若包 platform/arch 与本机一致，写 `data/state/upgrade-request`；
3. watchdog 每 2s 轮询请求：验签 → 暂存 `.new` → 停 worker →
   `exe→.old`、`.new→exe`（路径不变）→ 启动 → 20s 内 ready 且版本一致；
4. 失败自动 `.old` 回滚，保留 `.failed` 取证，`status=failed-rolled-back`。

状态查询：`GET /api/packages/pin` 返回
`{current, requested, status, lastError}`；
`DELETE` 取消尚未开始的请求（`applying` 阶段返回 409）。

### 5.2 引导部署（仅旧版本节点需要）

664 及更早的 watchdog 不识别 `upgrade-request`，必须**手动替换一次**
稳定路径二进制（自替换能力引导版），之后才能走 upload + activate。
手动部署时仍保持同一路径，不要解压到 `data/worker/phaethon-<ver>`。

### 5.3 备份与回滚

- 升级自动保留 `<exe>.old`、`<exe>.old2` 两版；
- 回滚：把 `.old` 恢复为稳定路径并重启，或在控制台激活旧版本包；
- 断电安全网：`data/state/upgrade-in-progress` marker + 启动修复；
  Windows 启动器 `start-phaethon.vbs` 也会在 exe 缺失时先恢复 `.old`。

### 5.4 部署验收

- 进程路径必须是稳定路径（Windows 两个进程都是 `phaethon.exe`）；
- `/api/packages/pin` 的 `current` 等于目标版本、`status=succeeded`；
- 升级后 `.old` 存在，且防火墙/AV 规则无需变更。

