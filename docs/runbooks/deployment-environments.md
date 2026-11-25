# 部署环境运维手册

> 版本: 0.1.3
> 日期: 2026-11-25
> 状态: ACTIVE
> 负责人: Phaethon Dev

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-09-10 | 初始版本 | Qoder |
| 0.1.1 | 2026-11-25 | 补充“停服务后等待旧进程退出”的重启纪律（qg 事故） | layer4 |
| 0.1.2 | 2026-11-25 | 取消“用户亲自发版”节点例外：全机群内建 SSH + pkg 自升级统一扩散到 v0.8.3-64 | layer4 |
| 0.1.3 | 2026-11-25 | 新增 §5.6：pre-1809 Windows 节点必须随部署目录放 winpty 资产（deskzq 实例） | layer4 |
| 0.1.4 | 2026-11-25 | §5.6 更新为 pkg 自动交付资产（watchdog 落盘）+ 部署介质兜底与两步上线顺序 | layer4 |

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

> 2026-11-25 更新：**所有节点都已有内建 SSH**，不存在“某个节点由用户亲自发版”的例外。
> 全部环境统一由我通过 pkg upload + activate 自升级完成；不要因为“停应用会断开 SSH”
> 而把节点排除在外——自替换流程本身就是为这个场景设计的（先起 watchdog 暂存 `.new`，
> 停 worker、换路径、再拉起，SSH 断开重连即可）。历史文档中
> “deskw 由用户亲自部署 / qgw 不由我部署”的说法已过期，仅作历史记录保留。

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
- 内嵌 SSH 在 Win7 上走 winpty（自动探测会跳过 PowerShell 2.0，选 cmd.exe / Git Bash）；
  2026-11-25 在 qgw 实测交互 PTY 与终端 resize 正常（`stty size` 24x80 → 40x120）；
  如需 PowerShell 交互，先安装 WMF 5.1 再按 `docs/plans/full-system-remediation-decisions.md`
  第 20 项做验证；
- 后续 Go 版本可能不再兼容 Win7；构建侧需继续锁定已知可用工具链。

### 5.6 PTY 资产：winpty（Windows 自动交付）

内建 SSH 的 PTY 选择顺序是 ConPTY（Win10 1809+）→ 部署目录自带的 winpty →
不可用降级（交互 shell 回落到内置 Bubble Tea shell）。资产有两条到达路径：

- **pkg 自升级（首选，2026-11-25 起）**：Windows amd64 的 pkg 内置
  `assets/winpty.dll` 与 `assets/winpty-agent.exe`，watchdog 在停 worker 后落盘到部署目录。
  ConPTY 可用且目录没有 winpty 时跳过（不污染部署目录）；ConPTY 不可用或目录已有
  winpty 时按清单更新。资产摘要写进 `meta.json`，受现有签名保护；校验/落盘失败只告警，
  不阻断升级与回滚。
- **首次安装 / 离线拷贝（兜底）**：部署介质随附 `winpty/` 目录（内容同
  `third_party/winpty/amd64/`，附 SHA256 清单）。`start-phaethon.vbs` 仅在目标缺失时复制，
  不覆盖运维手工放置的版本，复制失败继续启动。

```
<deploy>/winpty.dll
<deploy>/winpty-agent.exe
```

上线顺序（旧二进制不认识 `assets/` 条目）：

1. 先发布“接受资产”的版本（本改造），用**不带资产**的 pkg 把全机群升级到该版本
   （构建时设 `PHAETHON_PKG_NO_ASSETS=1`）；
2. 再构建**带资产**的 pkg 走正常 activate；ConPTY 不可用的节点升级后 PTY 自动恢复。
   第 2 步的 pkg 版本号必须与第 1 步不同（例如先提交本次上线记录再构建），
   否则同 `platform/arch/version` 的包会被对端去重、无法再次分发。

旧版本收到带资产 pkg 会直接校验失败（安全的失败：`failUpgradeBeforeSwap`，服务不受影响）。

- 补文件不会即时生效：`winptyLoadOnce` 是进程级缓存，必须重启 worker
  （无 OS SSH 节点用 `POST /api/restart`，worker 退出后 watchdog 原地拉起，watchdog 不退出）；
- 控制台 SSH 区块显示 PTY 徽标（ConPTY / winpty / 系统 PTY / 不可用 + 原因），
  `GET /api/ssh` 的 `pty` 字段返回同样的信息；
- 2026-11-25 deskzq（Server 2016）缺文件导致 PTY/resize 失败，详见
  `docs/issues/builtin-ssh-window-resize-20261124.md`；此后由 pkg 自动交付修复。


## 6. 自启动配置（2026-11-18）

### deskzq（Windows 桌面）

- 采用用户登录启动：`%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\PhaethonTUN.lnk`
  → `wscript.exe <部署目录>\start-phaethon.vbs`。
- `start-phaethon.vbs` 会设置 `CurrentDirectory` 到部署目录，并以隐藏窗口启动 `phaethon.exe`。
- 当前内置 SSH 为普通用户权限，`schtasks /SC ONLOGON` 创建任务被拒绝；因此使用 Startup 快捷方式。
  如需“最高权限”的计划任务，需要在管理员会话中创建并勾选“使用最高权限运行”。
- deskzq 的 TUN 当前为关闭（`enabled=false`），登录启动不需要管理员权限。

### vm（Windows，2026-11-24 起与桌面登录解耦）

- 计划任务 `PhaethonTUN`：`BootTrigger` 延迟 15s，执行
  `wscript.exe <部署目录>\start-phaethon.vbs`，`RunLevel=HighestAvailable`，
  无执行时限，失败重试 3 次/1 分钟。
- 运行身份 `HOSTNAME\<user>` + `LogonType=S4U`（“不管用户是否登录都运行”、不保存密码）。
  - 不要改成 `ServiceAccount`（SYSTEM）：内置 SSH 的 shell 继承 worker 身份，
    SYSTEM 下 HOME 变成 `systemprofile`、PATH 没有用户级目录，
    `tmux`（WinGet Links 里的 psmux 封装）直接 command not found。
  - S4U 保留用户配置与 PATH，同时不需要桌面登录；限制是令牌不带网络凭据，
    访问需要凭据的 SMB/共享会失败，本应用不依赖该能力。
- 注册方式（管理员）：导出任务 XML，把 `<UserId>` 改为 `HOSTNAME\<user>`、
  在 `<RunLevel>` 前插入 `<LogonType>S4U</LogonType>`，再
  `Register-ScheduledTask -TaskName PhaethonTUN -Xml $xml -Force`。
- 注意：`Stop-Process -Name phaethon` 会连带停掉 mesh，经 hostIP 的 OS SSH 会瞬断，
  重启脚本可能死在半途；要么用与本机不共命运的通道，要么用一次性计划任务执行重启。
- 验证（2026-11-24）：任务 `Ready / Docker / S4U / Highest`，watchdog+worker 均属
  `HOSTNAME\<user>`；内置 SSH `ssh <user>@vm.phn` 身份为 Docker、`PSMUX=[] TMUX=[]`、
  `tmux 3.3.3` 可 `new/list/attach`，原 `trae`/`trae2` 会话不受影响。

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

### 第二次事故（同日，必须记住）

第一次复盘后仍尝试了 `restart-once.cmd` + `timeout /t 15` + 分两条 SSH 杀进程，仍然失败：

- `timeout /t` 在非交互 SSH 子进程里不可靠；
- `cmd` 子进程可能随 phaethon 内置 SSH 会话一起被回收；
- 15 秒后没有拉起新进程，节点再次离线。

**硬规则：不要再远程杀无 OS SSH 节点上的 phaethon。**
`Start-Process cmd` / `timeout` / `Start-Job` 都不算可靠恢复路径。

### 唯一允许的远程重启方式

只有在**事先验证过**下面这条链路后，才允许杀进程：

1. 用当前用户创建一个一次性计划任务（`schtasks /sc once`），动作是
   `wscript.exe "<部署目录>\start-phaethon.vbs"`；
2. 先用一个无害任务（例如延迟 1 分钟写一个文件）验证：SSH 断开后任务仍会执行、
   且能在交互式会话里拉起进程；
3. 验证通过后，才创建真正的重启任务，并在任务触发前杀掉 phaethon；
4. 如果计划任务创建被策略拒绝，**不要杀**，直接让用户在桌面双击 Startup 快捷方式。

在任何不确定的情况下，优先请用户手动重启，不要赌。

### 重启后验收

两个 `phaethon` 进程、admin 443 可访问、`/api/packages/pin` 版本正确、内置 SSH 可登录；
缺一项就立刻人工介入。

### 人工恢复（节点已经离线时）

在节点当前登录的桌面会话执行：

```powershell
wscript.exe "<部署目录>\start-phaethon.vbs"
```

或注销/重新登录，让 Startup 快捷方式触发。节点只有内置 SSH 时，不要在未验证启动器的情况下
远程杀掉 phaethon。

## 8. 替换二进制后的“等待退出”纪律（2026-11-25 qg 事故）

### 事故经过

在 qg 手动替换二进制时，脚本是 `rc-service stop → pkill worker → mv → rc-service start`
一路串行执行。旧 worker 的优雅退出仍在收尾（关闭 p2p 会话、释放监听）时新实例就启动了：

- 新 worker 启动时发现 `data/` 仍被旧实例锁占用，直接退出（exit=3）；
- watchdog 判定“另一个实例持有 data 目录”，随之退出；
- 旧 worker 数秒后完成退出。结果：**进程全无，但 openrc 仍显示 started**，
  节点表现为“服务在跑却完全不可达”。

**结论：不是程序崩溃，而是重启时序问题。** `rc-service start` 返回 ok 只代表 launcher 起来了，
不代表 worker 拿到锁并存活。

### 规则

1. `stop` 之后必须**等待所有 phaethon 进程消失**再 `start`（watchdog、worker、
   `data/worker/` 下的版本化 worker 都算），轮询超时（例如 15s）就报错停下人工确认，
   不要盲目 start。
2. `start` 之后必须验证：进程数（watchdog+worker）、监听端口、健康接口，
   任一失败按“启动失败”处理，不要仅凭 service status 判定成功。
3. 不要在 SSH 单条命令里用 `pkill -f phaethon`：该模式会匹配到**当前远端 shell 自己**
   （命令文本里含 `phaethon`），把自己杀掉，后续 start 根本不会执行。
   使用更精确的模式（如 `data/worker/phaethon`）或按 pid 处理。
4. 需要“杀进程后自动拉起”时，优先走已验证的一次性计划任务/启动器，
   与本手册第 7 节一致。

### 启动失败后的恢复

确认无 phaethon 进程残留后再次 `start`，然后按第 7 节验收清单逐项确认。
