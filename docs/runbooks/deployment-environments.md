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

### 5.5 Windows 7（qgw）支持说明

- Windows 7 为 **best-effort**：构建使用 `make windows7` + `GO_LEGACY_WIN7`
  （见 Makefile），平台标识固定为 `windows7`；
- 内嵌 SSH 在 Win7 上仍为行级模式（PowerShell 2.0 + winpty 不可靠）；
  如需交互式 PTY，先安装 WMF 5.1 再按 `docs/plans/full-system-remediation-decisions.md`
  第 20 项做验证；
- 后续 Go 版本可能不再兼容 Win7；构建侧需继续锁定已知可用工具链。


## 6. 自启动配置（2026-11-18）

### deskzq（Windows 桌面）

- 采用用户登录启动：`%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\PhaethonTUN.lnk`
  → `wscript.exe <部署目录>\start-phaethon.vbs`。
- `start-phaethon.vbs` 会设置 `CurrentDirectory` 到部署目录，并以隐藏窗口启动 `phaethon.exe`。
- 当前内置 SSH 为普通用户权限，`schtasks /SC ONLOGON` 创建任务被拒绝；因此使用 Startup 快捷方式。
  如需“最高权限”的计划任务，需要在管理员会话中创建并勾选“使用最高权限运行”。
- deskzq 的 TUN 当前为关闭（`enabled=false`），登录启动不需要管理员权限。

### ms9 / ms10（Linux worker）

- 新增并启用 `phaethon.service`：
  - `User=migu`、`Group=migu`
  - `WorkingDirectory=/home/migu/phaethon`
  - `ExecStart=/home/migu/phaethon/phaethon`
  - `Restart=always`、`RestartSec=5`
- 原 `layer4-rules.service`（实际运行 `/home/migu/gy/trojan/layer4-go`，root）已 `disable --now`；
  旧进程已停止，旧二进制仍保留在磁盘，未删除。
- 验证：`systemctl is-enabled phaethon` = enabled，`is-active` = active，
  mesh 边 `gg ↔ ms9`、`gg ↔ ms10` 正常。

> 文档中的环境 IP 已替换为占位符；占位符到真实地址的映射保存在本地未跟踪的 `agents.md`
> （`.gitignore` 已忽略），仅供本地还原使用。

## 7. 无 OS SSH 节点的重启纪律（2026-11-19 事故）

### 事故经过

deskzq 是 Windows 桌面节点，只有 phaethon 内置 SSH，没有独立 OS SSH。
为验证 `PHAETHON_WARM_LANES`，远程执行了“延迟启动新进程 + 杀掉旧进程”的单条命令：

- `Start-Process` 的 `-ArgumentList` 嵌套引号被 PowerShell 解析坏，VBS 路径多了一个反斜杠，
  延迟启动**没有建立**；
- 同一条命令里后面的 `Stop-Process -Name phaethon -Force` 仍然执行，把 watchdog + worker
  一起杀掉；
- 节点没有 OS SSH，Startup 快捷方式只在登录时触发，无法自恢复，只能人工在桌面启动。

**结论：不是程序崩溃，是运维命令的失败路径没有兜底。**

### 强制流程（适用于 deskzq/qgw 等无 OS SSH 的 Windows 节点）

1. **先准备独立启动器，再杀进程**：
   - 在部署目录写一个 `restart-once.cmd`，内容只包含两行：
     `timeout /t 15 >nul` 和 `wscript.exe "<部署目录>\start-phaethon.vbs"`；
   - 用 `Start-Process -FilePath <部署目录>\restart-once.cmd -WindowStyle Hidden` 启动它；
   - 用 `Get-Process cmd` / 检查日志确认启动器进程已起来。
2. **启动器和杀进程分两条 SSH 执行**：
   - 第一条只负责 arm 启动器；
   - 第二条再执行 `Stop-Process -Name phaethon -Force`；
   - 这样即使第二条 SSH 因杀进程断开，第一条的启动器仍在运行。
3. **不要在单条命令里串联“创建启动器 + 杀进程”**；不要用多层嵌套引号的
   `-ArgumentList "/c ... \"...\""`，改用 `.cmd` 文件承载引号。
4. **杀之前确认恢复路径**：启动器文件存在、内容正确、15s 延迟足够；
   若节点有登录用户，也可以让用户直接双击 Startup 里的 `PhaethonTUN.lnk`。
5. **重启后验收**：两个 `phaethon` 进程、admin 443 可访问、`/api/packages/pin` 版本正确、
   内置 SSH 可登录；缺一项就立刻人工介入。

### 人工恢复（节点已经离线时）

在节点当前登录的桌面会话执行：

```powershell
wscript.exe "<部署目录>\start-phaethon.vbs"
```

或注销/重新登录，让 Startup 快捷方式触发。节点只有内置 SSH 时，不要在未验证启动器的情况下
远程杀掉 phaethon。
