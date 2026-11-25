# PTY 资产交付设计（winpty 兜底 + pkg 携带 + 能力自检）

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-11-25
- 关联文档：
  - [ssh-shell-configurable.md](ssh-shell-configurable.md)（内建 SSH shell 与 PTY 选型）
  - [../issues/builtin-ssh-window-resize-20261124.md](../issues/builtin-ssh-window-resize-20261124.md)（deskzq 缺资产事故）
  - [../runbooks/deployment-environments.md](../runbooks/deployment-environments.md) §5.6

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-11-25 | 初始版本：两段式资产交付（部署兜底 + pkg 携带）与能力自检 | layer4 |
| v0.2.0 | 2026-11-25 | 按方案完成实施（pkg/sign/watchdog/探测/API/页面/脚本/测试/文档），记录偏差 | layer4 |

## 1. 背景

### 1.1 现状

Windows 内建 SSH 的 PTY 选择顺序（`server/ssh_session_windows.go`）：

```
supportConPTY = conpty.IsConPtyAvailable()   // Win10 1809+
ptyAvailable() = supportConPTY || winptyReady()
```

`winptyReady()`（`server/winpty_windows.go`）从 `winptySearchDirs()`（exe 目录、上一级、上两级、cwd）
查找 `winpty.dll` + `winpty-agent.exe`，动态加载并做一次 `cmd /c exit 0` 端到端探针。结果由
`winptyLoadOnce` / `winptyProbeOnce` 进程级缓存，**补文件后必须重启 worker 才生效**。

pkg 格式（`pkg/signing/signing.go`）只允许三个条目：`binary`、`meta.json`、`signature`；
签名覆盖 `sha256(binary || meta.json)`。因此：

- pkg 升级只替换可执行文件，**不带资产**；
- ConPTY 不可用的节点（Win7/8、Server 2016 及更早）如果部署目录没有 winpty 两个文件，
  `pty-req` 被拒绝，客户端看到 `PTY allocation request failed on channel 0`，
  交互 shell 静默降级到内置 Bubble Tea shell；
- 2026-11-25 deskzq（Server 2016 / 10.0.14393）即为此情况，手工复制两个文件 + `POST /api/restart`
  后才恢复 PTY 与 resize。

### 1.2 要解决的问题

1. 新节点首次部署时，运维不知道还要额外放两个 DLL，缺了也不易发现；
2. pkg 自升级链路无法把节点从“缺资产”状态修复回来，必须人工介入；
3. 当前没有任何界面/接口能看出本节点 PTY 走的是 ConPTY、winpty 还是不可用。

## 2. 目标与非目标

### 2.1 目标

1. **按需**交付资产：Windows 目标才带，运行时只有 ConPTY 不可用（或资产已存在需更新）才落盘；
   Linux / Darwin 完全不涉及。
2. **不阻断**：资产缺失、校验失败、落盘失败，都不得阻断节点启动、升级、回滚，只降级并告警。
3. **不新增投递通道**：资产走现有 pkg 签名/上传/激活链路；不自建第二套下载或分发机制。
4. **不内嵌 exe**：资产保持独立文件（AV/防火墙按文件名放行的既有行为不变，exe 体积不变）。
5. **可观测**：`/api/ssh` 与控制台 SSH 页面能看到当前 PTY 能力、资产状态与降级原因。

### 2.2 非目标

- 不改变 ConPTY → winpty → 内置 shell 的既有选型顺序；
- 不改变 shell 选择、编码、环境变量过滤等既有行为；
- 不做“资产热加载”：补文件后仍需重启 worker（沿用 `winptyLoadOnce` 语义）；
- 不引入 winpty 之外的新 PTY 实现。

## 3. 总体设计

两段互补，覆盖两条不同的到达路径：

| 场景 | 资产来源 | 归属 |
|------|----------|------|
| 首次安装 / 离线拷贝部署 | 部署介质随附 `winpty/` 目录，安装脚本按需复制 | 运维脚本（失败不阻断） |
| 已装机群自升级 | pkg 的 `assets` 清单 + watchdog 落盘 | 应用（自替换链路） |

```
       首次部署                                  pkg 自升级
┌──────────────────────┐                 ┌───────────────────────────┐
│ 部署介质              │                 │ .pkg                      │
│  phaethon.exe        │                 │  binary                   │
│  winpty/             │                 │  meta.json (+assets 清单) │
│    winpty.dll        │                 │  assets/winpty.dll        │
│    winpty-agent.exe  │                 │  assets/winpty-agent.exe  │
└─────────┬────────────┘                 └────────────┬──────────────┘
          │ 安装脚本按需复制（失败仅告警）              │ watchdog 停 worker 后原子落盘
          ▼                                           ▼
                 <deploy>/winpty.dll + winpty-agent.exe
                                   │
                                   ▼
                    运行期自检 → 控制台 / 日志可见
```

## 4. 按需策略

### 4.1 构建期：按平台决定是否进包

| 目标平台 | ConPTY 预期 | pkg 带 winpty | 说明 |
|----------|-------------|---------------|------|
| `windows` / amd64、arm64 | 1809+ 可用，更早不可用 | 是（约 640KB） | 同一 pkg 需覆盖 Server 2016 等旧系统 |
| `windows7` | 不可用 | 是 | 必须 |
| `linux` / `linux-arm64` | 不适用 | 否（`assets` 为空） | zip 内无 `assets/` |
| `darwin` / `darwin-arm64` | 不适用 | 否 | 同上 |

构建脚本按平台显式传入资产；`sign` 工具不自行猜测。

### 4.2 运行期：按能力决定是否落盘

watchdog 在安装资产前判断（仅在 Windows 目标）：

```
needInstall = !conptyAvailable() || 资产已存在于部署目录（视为需要更新）
```

- ConPTY 可用且部署目录没有 winpty：跳过落盘，不污染部署目录；
- ConPTY 可用但目录里已有 winpty：按 pkg 清单更新（保持兜底能力）；
- ConPTY 不可用：必须落盘；失败只告警，保持现状（可能仍缺资产，PTY 降级）；
- Linux / Darwin：`assets` 为空直接跳过，代码路径不进入。

## 5. pkg 格式扩展

### 5.1 元数据

```json
{
  "version": "v0.8.3-70-gxxxxxxx",
  "platform": "windows",
  "arch": "amd64",
  "buildTime": "...",
  "gitCommit": "...",
  "goVersion": "...",
  "assets": [
    {"name": "winpty.dll",       "size": 279552, "sha256": "5d89..."},
    {"name": "winpty-agent.exe", "size": 361472, "sha256": "84f6..."}
  ]
}
```

- `assets` 省略或为空 = 不带资产（Linux/Darwin、旧构建）；
- `name` 只允许部署目录内的**纯文件名**（白名单：`winpty.dll`、`winpty-agent.exe`），
  禁止路径分隔符、`.`、`..`、绝对路径；
- 哈希写进 `meta.json`，因此天然落在现有签名覆盖范围内，**签名算法不变**。

### 5.2 zip 条目

```
binary
meta.json
signature
assets/winpty.dll
assets/winpty-agent.exe
```

校验规则（`VerifyPkg` / `VerifyPkgBytes` / `VerifyPkgMeta` 三处保持一致）：

1. 白名单从“三个固定名”放宽为“三个固定名 + `assets/<白名单文件名>`”；
2. `assets/` 条目必须出现在 `meta.json` 的 `assets` 清单里，名字、大小、SHA256 全部匹配；
3. 清单里的资产必须在 zip 中存在（缺失即校验失败）；
4. 单文件上限 16MiB、合计上限 32MiB，防止 zip 炸弹；
5. `PkgContents` 增加 `Assets map[string][]byte`；`VerifyPkgMeta` 只流式校验哈希、不保留内容。

### 5.3 sign 工具

新增可重复参数：

```
-asset winpty.dll=third_party/winpty/amd64/winpty.dll
-asset winpty-agent.exe=third_party/winpty/amd64/winpty-agent.exe
```

`sign` 读取文件、计算 size/sha256、填入 `meta.json` 后再签名并写入 zip。
`scripts/build-pkg.sh` 对 `windows` / `windows7` 自动追加这两条；其它平台不传。

## 6. watchdog 落盘设计

现有升级时序（`upgrade_apply.go`）已经具备安全窗口：

```
findPkgForVersion → stageBinary(.new)
  → prepareBackup / WriteMarker
  → stopChild            ← worker 退出，DLL 不再被映射
  → swapBinary(ReplaceFileW)
  → handoffToNewWatchdog
```

资产文件同样在 `stopChild` 之后替换，天然避开“DLL 被占用无法覆盖”的问题。

### 6.1 新增步骤

```
阶段一（停 worker 之前，失败即放弃升级，服务不受影响）
  verify pkg + 校验每个资产 sha256
  写 <deploy>/<name>.asset-new（fsync）
  marker 追加 assets: [{name, new, backup, sha256}]

阶段二（stopChild 之后）
  for each asset（needInstall 才处理）:
    if 目标存在: rename <name> → <name>.asset-old
    rename <name>.asset-new → <name>
    失败: 记录 upgrade.log 警告，继续
  swapBinary(...)      ← 与现在一致
  handoffToNewWatchdog
```

### 6.2 失败与回滚边界

| 失败点 | 处理 |
|--------|------|
| 停 worker 前校验/暂存失败 | `failUpgradeBeforeSwap`，worker 保持不变 |
| 资产替换失败 | 仅告警；继续换 exe；新版本可能仍无 PTY，但版本升级成功 |
| exe 替换失败 | 走现有回滚，只恢复 exe；**不回滚资产**（资产与版本无关，新旧二进制共用） |
| 资产替换后、exe 替换前崩溃 | marker 中 `.asset-new` 残留；启动修复时删除残留，保留当前已生效资产 |
| 资产被 AV 隔离 | 下次升级重新落盘；失败不影响主流程 |

资产兼容性由“文件内容与版本解耦”保证：winpty 0.4.3 对所有目标版本一致，
因此不需要和 exe 一起回滚。

### 6.3 marker 扩展

```go
type Marker struct {
    Old       string        `json:"old"`
    New       string        `json:"new"`
    Version   string        `json:"version"`
    StartedAt time.Time     `json:"startedAt"`
    Assets    []AssetMarker `json:"assets,omitempty"`
}
type AssetMarker struct {
    Name   string `json:"name"`
    New    string `json:"new"`
    Backup string `json:"backup,omitempty"`
}
```

`repairInterruptedUpgrade` 只在 exe 缺失时介入；资产残留一律清理、不做恢复，
避免把“半套资产”当成失败升级处理。

## 7. 部署侧兜底（首次安装）

1. 部署介质新增 `winpty/` 目录（内容同 `third_party/winpty/amd64/`，附 SHA256 清单）。
2. 安装/启动脚本逻辑：
   - 目标平台非 Windows → 跳过；
   - 部署目录已有同名文件 → 跳过（不覆盖运维手工放置的版本）；
   - 否则复制并按清单校验 SHA256；
   - 复制/校验失败：写日志告警，**继续启动**，退出码仍为成功。
3. 运行期自检作为最终兜底：缺资产时控制台直接显示“PTY 不可用 / 缺少 winpty 资产”，
   而不是让用户去猜 `PTY allocation request failed` 的含义。

## 8. 能力自检与可观测性

### 8.1 探测函数

`server` 包新增（跨平台）：

```go
type PTYCapability struct {
    Mode             string // "conpty" | "winpty" | "unavailable" | "pty"（unix）
    ConPTYAvailable  bool
    AssetsPresent    bool
    AssetsValid      bool
    Detail           string // 降级/失败原因，供日志与页面展示
}
func PTYCapabilityInfo() PTYCapability
```

- Windows：复用 `supportConPTY` / `winptyLoad()` / `winptyReady()` 的缓存结果，
  不额外触发探针进程；
- Unix：返回 `Mode="pty"`；
- 资产哈希校验以构建期清单为准（只校验存在性与已知哈希，不扫描目录）。

### 8.2 API 与页面

`GET /api/ssh` 增加：

```json
"pty": {
  "mode": "unavailable",
  "conptyAvailable": false,
  "assetsPresent": false,
  "assetsValid": false,
  "detail": "ConPTY unavailable; winpty.dll/winpty-agent.exe missing"
}
```

控制台 SSH 区块新增一行状态徽标：

| 状态 | 展示 |
|------|------|
| `conpty` | 绿色 “PTY: ConPTY” |
| `winpty` | 绿色 “PTY: winpty” |
| `unavailable` | 黄色 “PTY 不可用（缺少 winpty 资产 / 探针失败）”，附 `detail` 与“补资产后需重启 worker”提示 |
| `pty`（Unix） | 绿色 “PTY: 系统 PTY” |

## 9. 上线顺序（重要）

旧二进制（≤ v0.8.3-64）的 `VerifyPkg` 白名单不认识 `assets/` 条目，收到带资产的 pkg 会直接
校验失败（安全的失败：`failUpgradeBeforeSwap`，服务不受影响）。因此分两步：

1. **先发“接受资产”的版本**（本方案代码），用**不带资产**的 pkg 把全机群升级到该版本；
2. 再构建**带资产**的 pkg，走正常 activate；deskzq 这类节点升级后 PTY 自动恢复，无需手工复制。

不做旧 pkg 兼容代码（新二进制仍接受无 `assets` 的旧 pkg，属自然兼容）。

## 10. 验收标准

1. deskzq（Server 2016，无 winpty）通过带资产 pkg 自升级后，`ssh -tt` 获得真实 PTY，
   `stty size` 正常，window-change 生效，**全程无人工复制/重启**。
2. vm / deskw（ConPTY 可用）升级后部署目录不新增 winpty 文件（未预置时），PTY 仍为 ConPTY。
3. qgw（Win7，预置 winpty）升级后资产哈希与 pkg 清单一致。
4. Linux / Darwin pkg 的 `meta.json` 无 `assets` 字段，zip 内无 `assets/`。
5. 篡改资产字节 → pkg 校验失败，worker 继续运行，状态为 `failed_rolled_back` 且原因明确。
6. 资产替换被占用/拒绝 → exe 升级仍成功，`upgrade.log` 有警告，控制台显示 PTY 降级。
7. 控制台 SSH 页面在四类节点上分别显示正确的 PTY 徽标。

## 11. 任务拆分

| # | 内容 | 主要文件 |
|---|------|----------|
| 1 | pkg 元数据/校验/打包扩展 | `pkg/signing/signing.go`、`pkg/signing/cmd/sign/main.go` |
| 2 | 构建脚本按平台带资产 | `scripts/build-pkg.sh` |
| 3 | watchdog 暂存/落盘/marker/修复 | `upgrade_apply.go`、平台辅助文件、`pkg/upgrade/upgrade.go` |
| 4 | PTY 能力探测与 API | `server/ssh_session_*.go`、`admin/ssh.go` |
| 5 | 控制台状态展示 | `admin/templates/admin.html` |
| 6 | 单元测试 | 签名往返/篡改/白名单/大小上限；落盘顺序与失败不阻断 |
| 7 | 文档 | 本文件、运维手册 §5.6 |

## 12. 风险

- **DLL 占用**：只在 worker 退出后替换；若未来有常驻子进程持有 `winpty.dll`，替换会失败并降级，
  需要通过 `upgrade.log` 观察。
- **AV 隔离**：资产可能被拦截；哈希不匹配时拒绝落盘，不影响主升级。
- **旧节点**：必须先完成第 9 节第一步，否则带资产 pkg 会被拒绝（安全失败，但升级不生效）。
- **探针缓存**：补资产后未重启 worker 时页面仍显示不可用，属预期；提示文案需明确“重启生效”。

## 13. 实施记录（2026-11-25）

| # | 状态 | 落地位置 |
|---|------|----------|
| 1 | 完成 | `pkg/signing/signing.go`（`AssetMeta`、assets 清单、三处校验统一走 `classifyPkgEntry`、`CreatePkgWithAssets`）、`pkg/signing/cmd/sign/main.go`（可重复 `-asset`） |
| 2 | 完成 | `scripts/build-pkg.sh`：windows amd64 自动追加 `winpty.dll` / `winpty-agent.exe` 两条 `-asset` |
| 3 | 完成 | `upgrade_apply.go`（停 worker 前暂存、停后落盘、失败不阻断）、`upgrade_assets*.go`、`pkg/upgrade/upgrade.go`（`Marker.Assets`） |
| 4 | 完成 | `server/pty_capability.go` + `ssh_session_windows.go` / `ssh_session_unix.go`、`admin/ssh.go`（`pty` 字段） |
| 5 | 完成 | `admin/templates/admin.html`：PTY 徽标、不可用原因与“重启生效”提示 |
| 6 | 完成 | `pkg/signing/signing_test.go`（往返/篡改/白名单/越界/缺失）、`upgrade_assets_test.go`（暂存-安装顺序、摘要不匹配、残留清理） |
| 7 | 完成 | 本文件、`docs/runbooks/deployment-environments.md` §5.6 |

与设计的偏差：

- 部署侧 `start-phaethon.vbs` 的兜底只做“目标缺失才复制”，不覆盖已有文件；
  部署介质的 SHA256 校验放在准备阶段，未在 VBS 内实现（pkg 路径仍逐文件校验摘要）。
- 运行期 `AssetsValid` 表示两个文件存在；摘要正确性由 pkg 安装时的校验保证，
  运行期不重复扫描/哈希（探针结果通过 `Detail` 暴露）。

上线严格按第 9 节两步执行。
