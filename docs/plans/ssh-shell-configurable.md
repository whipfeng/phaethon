# 内建 SSH 服务默认 Shell 可配置设计

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-11-08
- 关联文档：[mesh-integrated-ssh.md](mesh-integrated-ssh.md)（内建 SSH v1 方案）

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-11-08 | 初始版本 | layer4 |

## 1. 背景与目标

### 1.1 当前问题

内建 SSH 服务（`server/ssh.go` + `ssh_session_*.go`）在拉起 shell 时使用**硬编码自动探测**：

- Windows（`server/ssh_session_windows.go:74-84`）：`pwsh` → `powershell` → `cmd`，依赖 `exec.LookPath` 在 phaethon 进程自己的 PATH 里查
- Unix（`server/ssh_session_unix.go`）：`$SHELL` 空则 `/bin/sh`

实际暴露的两个痛点：

1. **VM（Windows）用户期望用 Git Bash**：宿主机 OS sshd 默认 shell 配的是 Git Bash（`C:\Program Files\Git\bin\bash.exe`），但经 `ssh vm.phn` 进入内建 SSH 拿到的是 PowerShell 5.1——因为 Git Bash 不在 defaultShell 候选里，且 ps7 装在 `C:\Program Files\PowerShell\7\` 但 phaethon 进程 PATH 缺这一条 → `exec.LookPath("pwsh")` 失败回退到系统 5.1。
2. **PATH 环境差异不可控**：phaethon 启动方式多样（计划任务 / Start-Process / 手动 nohup / OpenRC），PATH 不一定继承用户登录环境；用户在 console 里 `where pwsh` 找得到，但 phaethon 找不到。

对比：经 `ssh loc.vm.phn`（loopback resolver → OS sshd）拿到的是 Git Bash（用户在 OS 配的），而 `ssh vm.phn`（内建 SSH）拿到 PowerShell 5.1，体验割裂。

### 1.2 目标

1. **db 持久化默认 shell 配置**：用户可在 admin UI 给本节点指定 shell 路径（绝对路径或 PATH 内命令名），内建 SSH 启动会话时优先使用。
2. **空值回退自动探测**：未配置时保持现有 pwsh/powershell/cmd 自动探测行为不变，对存量节点零干扰。
3. **Unix/Windows 行为一致**：均支持 db.Shell 覆盖；Unix 端虽 `$SHELL` 通常够用，但提供一致性配置面。
4. **API/UI 完整闭环**：admin 设置页 SSH 区块加输入框 + 保存，与 enabled 开关走同一接口。

## 2. 现状与机制

### 2.1 当前调用链

```
ssh <nodeid>.phn:22
  → handler/connection_handler.go (localNodeDomain 分流，dstPort==22 → SSHServer.ServeConn)
  → server/ssh.go handleSession
    → 收 pty-req / env / window-change / shell / exec
    → startSessionProc(cmdline, isShell, usePty, cols, rows)
      → defaultShell()  ← 【改动点】硬编码探测
      → startConptyShell / startConpty / startPipe (Windows)
      → startPtyShell / startPtyExec (Unix)
```

### 2.2 现有 db schema

- `db.BucketSSH` 下 `sshConfigItem = []byte("config")` 存 `SSHConfig{Enabled bool}`（`db/ssh.go:20-22`）
- 公钥独立 bucket `BucketSSHKeys` 按指纹存

扩展方向：`SSHConfig` 加 `Shell string` 字段，与 `Enabled` 同 bucket 同 key 存储——JSON 反序列化对旧数据缺失字段零值（空字符串）天然兼容，无需迁移。

### 2.3 现有 admin API

- `GET /api/ssh` → `{enabled, hostKeyFingerprint, keys[]}`（`admin/ssh.go:17-49`）
- `PUT /api/ssh/config` ← `{enabled}`（`admin/ssh.go:52-71`）

扩展方向：GET 响应加 `shell`；PUT body 加 `shell`（与 `enabled` 一并提交）。

## 3. 关键设计决策

### 3.1 Shell 值语义

- **空字符串 `""`**：走自动探测（现状行为），是 db 缺省 / 旧数据默认值。
- **非空**：作为命令行直接用。
  - Windows：绝对路径（`C:\Program Files\Git\bin\bash.exe`）或 PATH 内名字（`pwsh`）。
  - Unix：绝对路径（`/usr/bin/zsh`）或 PATH 内名字（`zsh`）。
- **不预先校验存在性**：用户保存即写库，失败时延迟到会话建立时报错（`startSessionProc` 返回 error → `channel.Stderr().Write("failed to start process: ...")` 现有路径已能透出）。**决策依据**：shell 路径可能在会话建立时因挂载、容器或 PATH 调整而变化；保存期校验容易误拒绝。错误透出现有机制已覆盖（`server/ssh.go:212-215`）。

### 3.2 exec 模式参数适配

`execArgs(shell, cmdline)` 当前按 shell 名分发（`ssh_session_windows.go:87-93`）：
- 含 `powershell`/`pwsh` → `-Command <cmd>`
- 其他 → `/s /c <cmd>`（cmd 风格）

新增 bash/zsh 分支：
- 含 `bash`/`sh`/`zsh` → `-c <cmd>`（POSIX 风格）

Unix 端 `ssh_session_unix.go` 直接 `cmd = exec.Command(shell, "-c", cmdline)`，无需按名分发。

### 3.3 UI 字段布局

放在现有「SSH 服务」区块 `enabled` 开关之下、Host Key 指纹之上，加一个文本输入框：
- Label：`默认 Shell（留空 = 自动探测）`
- Placeholder：`例：C:\Program Files\Git\bin\bash.exe 或 /usr/bin/zsh`
- 保存：与 enabled 开关同一 PUT 接口（任何字段变更触发整体提交，保持幂等）

### 3.4 Unix 端是否一并支持

**做**。一致性优先：admin UI 一个输入框对所有平台有效，unix 端覆盖 `$SHELL` 行为不破坏现状（留空仍走 `$SHELL`→`/bin/sh`）。

### 3.5 反向兼容

- 旧版本 phaethon 读到带 `shell` 字段的 db JSON：标准 `encoding/json` 忽略未知字段，`Enabled` 正常反序列化，shell 自动探测行为不变。
- 新版本读到旧 db JSON（无 `shell` 字段）：`Shell` 零值 `""` → 自动探测。

无需数据迁移。

### 3.6 版本号与热更新语义

- `BucketSSH` **不在 `db/version_notify.go:skipBuckets`** → 写 `PutSSHConfig` 自动 `BumpVersion("ssh", ...)` + SSE 推送，无需额外代码。
- 内建 SSH 是本节点本地状态（plan `mesh-integrated-ssh.md`），无跨节点同步需求，后端无 listener 消费 `ssh` topic。
- **生效粒度 = 新会话**：`db.GetSSHConfig()` 在 `ServeConn` 每次新连接时调用（`server/ssh.go:75`），已建立的会话 shell 进程已拉起，不换、也不需要换；新会话拿到新配置。

### 3.7 前端 SSE 订阅

admin.html 的 SSH 区块现状是 IIFE 内 `loadSSH()` 仅首次 + 增删后手动调用，**未订阅版本号**——多标签页 / 多设备同时改配置时本页不刷新。本次顺手补齐：

- 在 SSH IIFE 内注册 `onBusinessVersion('ssh', loadSSH, 'admin-ssh-block')`（参照 `admin/static/app.js:657` 的注册范式）。
- 效果：本页 PUT 触发版本号递增 → SSE 推回本页 → `loadSSH` 自动重跑；他端改动也能即时反映到本页。
- 注意 `loadSSH` 是 async，`onBusinessVersion` 的 `runSub` 已 `await sub.handler()`，本地版本号在成功后才推进——失败会重试，不会丢更新。

## 4. 接口/交互调整

### 4.1 db 层

```go
// db/ssh.go
type SSHConfig struct {
    Enabled bool   `json:"enabled"`
    Shell   string `json:"shell,omitempty"` // 空 = 自动探测
}
```

### 4.2 server 层

```go
// server/ssh_session_windows.go / ssh_session_unix.go
func startSessionProc(cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
    shell := resolveShell()  // 新函数：先 db.Shell，再 autoDetectShell()
    ...
}

func resolveShell() string {
    if cfg, err := db.GetSSHConfig(); err == nil && cfg.Shell != "" {
        return cfg.Shell
    }
    return autoDetectShell()  // 原 defaultShell 改名
}
```

### 4.3 admin API

```http
GET /api/ssh
← { enabled, shell, hostKeyFingerprint, keys[] }

PUT /api/ssh/config
→ { enabled, shell }
```

### 4.4 admin UI

```html
<!-- admin/templates/admin.html SSH 区块内新增 -->
<div class="form-group">
  <label data-i18n="admin.sshShell">默认 Shell（留空 = 自动探测）</label>
  <input type="text" id="ssh-shell" class="form-control"
         placeholder="C:\Program Files\Git\bin\bash.exe 或 /usr/bin/zsh"
         data-i18n-placeholder="admin.sshShellPh">
</div>
```

JS（`admin/static/app.js` 或 admin.html 内联 `loadSSH` / `ssh-enabled` 提交函数）：把 `shell` 字段一并读 / 写。

## 5. 迁移与兼容性

- **db schema**：JSON 加字段，旧数据零值，无需迁移。
- **API 兼容**：旧版 admin 客户端 PUT 仅带 `enabled` 不带 `shell` → `shell` 零值 `""` → 自动探测；不破坏旧 UI。
- **部署顺序**：本改动向后兼容，按常规 VM→QG→生产滚动即可，无特殊要求。

## 6. 风险与回退

| 风险 | 影响 | 缓解 |
|------|------|------|
| 用户配错 shell 路径导致会话无法建立 | 中 | 现有 `startSessionProc` 错误透出到 stderr + exit-status 255；UI placeholder 给范例；提示"留空恢复自动" |
| Windows ConPTY 对 bash 的兼容性 | 低 | ConPTY 是 OS 级伪终端，bash on MINGW64 在 ConPTY 下正常工作；既有 defaultShell  candidate `pwsh` 也是 ConPTY，机制一致 |
| `execArgs` 未覆盖新 shell 类型 | 低 | 按 §3.2 补 `bash`/`sh`/`zsh` 分支即可；fallback 维持 cmd 风格（与现状一致） |
| Unix 端改动引入回归 | 低 | 留空时仍走 `$SHELL` → `/bin/sh`，与现状完全一致；仅在用户主动配置后启用新路径 |

回退：清空 admin UI 的 shell 字段保存即回到自动探测；或退回上一二进制（prev-running 纪律）。

## 7. 验收标准

- [ ] db 写入/读取 `Shell` 字段正确；旧数据（无 shell 字段）反序列化 `Shell=""` 走自动探测
- [ ] VM（Windows）admin UI 把 shell 配为 `C:\Program Files\Git\bin\bash.exe` 后，`ssh vm.phn` 拿到 Git Bash 提示符
- [ ] 配为 `pwsh` 后，`ssh vm.phn` 拿到 PowerShell 7（前提：pwsh 在 PATH 或配绝对路径）
- [ ] 留空保存后，`ssh vm.phn` 回到 PowerShell 5.1（自动探测结果，与改动前一致）
- [ ] `ssh vm.phn 'some command'`（exec 模式）三种配置下参数拼接正确（PowerShell `-Command` / bash `-c` / cmd `/s /c`）
- [ ] Unix 节点（QG）配 `/usr/bin/zsh` 后 `ssh qg.phn` 拿到 zsh；留空回到 `$SHELL`
- [ ] 配错路径时 `ssh vm.phn` 报 `failed to start process: ...` 且 exit-status 255，不会卡死
- [ ] `go build ./...` + `go test ./server/ ./db/ ./admin/` 全过

## 8. 后续（本期不做）

- shell argv 模板（如 `bash --login -i`）的支持
- 跨节点 shell 配置全局同步（与公钥全局同步一起规划）
- per-key shell 覆盖（不同公钥不同 shell，结合二期 per-key 命令白名单）
