# 控制台发布包管理（Admin Package Management）

## 元数据

- 文档类型：Plan
- 版本：0.3.2
- 所属项目：phaethon
- 创建日期：2026-09-21
- 状态：已评审通过（2026-09-22，简化为文件管理 + 签名验证，为跨节点分发打基础）

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-09-21 | 初始设计 | Qoder |
| 0.2.0 | 2026-09-21 | 评审结论：采纳 D1-D7 推荐方案；D6 升级为**彻底移除 P2P 二进制分发**（非仅禁用）；admin 直接 import p2p（已有依赖，无需回调）；`--version` 判断移到 watchdog 模式之前；probe 以 `PHAETHON_WORKER=1` 环境变量运行以兼容旧版二进制 | Qoder |
| 0.3.0 | 2026-09-22 | **重大简化**：移除本地自更新流程（不替换运行中程序）；移除 probe 执行（元数据由签名保证可信）；移除 sha256 单独校验（签名包含完整性）；移除缓存区 UI（p2p-cache）；改为 `.pkg` 打包格式（zip：binary + meta.json + signature）；Ed25519 签名验证；"发布"仅为标记状态，为跨节点分发打基础 | Qoder |
| 0.3.1 | 2026-09-22 | 移除 403 认证门控：签名验证已提供足够安全保障（只有私钥持有者能创建有效包），无需额外的 `AuthEnabled` 或 `package-upload-insecure` 检查。简化配置，符合"签名即信任"理念，为跨节点分发扫清障碍 | Qoder |
| 0.3.2 | 2026-09-22 | 所有构建信息（version、platform、arch）必须通过 `-ldflags -X` 在编译时注入，不允许运行时检测。`phaethon --version` 输出 `version=X platform=Y arch=Z` 格式供外部探测。meta.json 移除 `buildTag` 字段（windows7 直接作为 platform 值） | Qoder |

---

## 1. 背景与目标

### 1.1 当前问题

phaethon 部署在 6 个环境（QG/VM/GG/JF/MS9/MS10），升级流程全靠手动：本地编译 → scp 上传 → ssh 停止/替换/启动。步骤多、易错。

P2P 库存式自动分发已彻底移除（v0.2.0 决策），需要一个**简单的文件管理系统**作为跨节点分发的基础。

### 1.2 目标

1. Admin 控制台新增「发布包」页面：**文件管理**（上传、列表、下载、删除）
2. 每个文件有**发布状态**（标记哪个是"当前活跃版本"）
3. **签名验证**：Ed25519 签名保证文件来源可信 + 完整性
4. **元数据提取**：从签名覆盖的 meta.json 获取版本、平台、架构等信息（不执行二进制）
5. **不立即替换本地程序**："发布"只是打标记，不触发自更新（自更新是独立议题，后续设计）
6. **为跨节点分发打基础**：其他节点可以拉取"已发布"的文件

### 1.3 非目标（v1）

- 本地自更新（替换运行中程序）
- 跨节点自动分发（本期只做文件管理，后续基于"已发布"标记实现）
- 配置文件上传
- 回滚机制

### 1.4 设计原则

- **简单优先**：文件管理 + 签名验证，不过度设计
- **安全第一**：Ed25519 签名同时保证来源可信 + 完整性
- **不执行不可信二进制**：元数据从签名覆盖的 meta.json 读取，不 probe

---

## 2. 打包格式：.pkg

### 2.1 格式定义

`.pkg` 文件是 zip 格式，包含三个文件：

```
phaethon_{platform}_{arch}_{version}.pkg (zip)
├── binary        (可执行文件)
├── meta.json     (元数据)
└── signature     (Ed25519 签名)
```

**命名规则**：`phaethon_{platform}_{arch}_{version}.pkg`

- `platform`：`linux` / `windows` / `windows7` / `darwin`
- `arch`：`amd64` / `arm64`
- `version`：由 `git describe --tags --always --dirty` 生成
  - 正式版本：`v1.0.0`（打 tag 时）
  - 开发版本：`v0.1.0-mesh-140-g8dba21c`（tag-提交数-gcommit hash）
  - 无 tag：`g8dba21c`（只有 commit hash）
  - 有未提交改动：末尾加 `-dirty`

**示例**：
- `phaethon_linux_amd64_v1.0.0.pkg` - Linux amd64 正式版
- `phaethon_linux_amd64_v0.1.0-mesh-140-g8dba21c.pkg` - Linux amd64 开发版
- `phaethon_windows_amd64_v1.0.0.pkg` - Windows 10+ amd64
- `phaethon_windows7_amd64_v1.0.0.pkg` - Windows 7 兼容版（用旧编译器构建）
- `phaethon_darwin_arm64_v1.0.0.pkg` - macOS Apple Silicon

**编译产物目录结构**（未打包）：
```
dist/
├── linux-amd64/phaethon          ← 按 {os}-{arch}/ 目录区分
├── linux-arm64/phaethon
├── windows-amd64/phaethon.exe    ← Windows 10+ 版本
├── windows7-amd64/phaethon.exe   ← Windows 7 兼容版（用旧编译器构建）
├── windows-arm64/phaethon.exe
├── darwin-amd64/phaethon         ← macOS Intel
└── darwin-arm64/phaethon         ← macOS Apple Silicon
```

打包时，从 `dist/{platform}-{arch}/` 取出二进制，配合 meta.json 和 signature，生成 `phaethon_{platform}_{arch}_{version}.pkg`。目录名与 .pkg 的 platform 字段一致。

### 2.2 meta.json 结构

```json
{
  "version": "v1.0.0",
  "platform": "linux",
  "arch": "amd64",
  "buildTime": "2026-09-22T10:00:00Z",
  "gitCommit": "abc123def",
  "goVersion": "go1.21.0"
}
```

### 2.3 签名机制

- **签名算法**：Ed25519（现代、快、安全）
- **签名对象**：`sha256(binary + meta.json)`（对拼接后的内容算 hash，再签名）
- **签名文件**：base64 编码的 Ed25519 签名
- **私钥**：通过 `scripts/.env` 中的 `PHAETHON_SIGNING_KEY` 指定路径，**不 git 跟踪**
- **公钥**：硬编码在 `pkg/signing/signing.go` 的 `PublicKey`（git 跟踪）

**私钥配置**（在 `scripts/.env` 中）：
```bash
PHAETHON_SIGNING_KEY=/path/to/private.key
```

### 2.4 编译命令（后续实现）

```bash
phaethon build --version v1.0 --platform linux --arch amd64 --sign-key key.pem
→ 输出：phaethon-v1.0-linux-amd64.pkg
```

### 2.5 构建信息注入（已实现）

所有构建信息必须通过 `-ldflags -X` 在编译时注入，**不允许运行时检测**（如 `runtime.GOOS`）：

```go
// main.go
var (
    Version  = "dev"  // -X main.Version=$(GIT_TAG)
    Platform = ""     // -X main.Platform=linux
    Arch     = ""     // -X main.Arch=amd64
)
```

**Makefile 示例**：
```makefile
linux:
    GOOS=linux GOARCH=amd64 go build -ldflags "-s -w \
      -X main.Version=$(GIT_TAG) \
      -X main.Platform=linux \
      -X main.Arch=amd64" \
      -o dist/linux-amd64/phaethon .

windows7:
    $(GO_LEGACY_WIN7) build -ldflags "-s -w \
      -X main.Version=$(GIT_TAG) \
      -X main.Platform=windows7 \
      -X main.Arch=amd64" \
      -o dist/windows7-amd64/phaethon.exe .
```

**探测命令**：
```bash
$ phaethon --version
version=v0.1.0-mesh-144-g9e5fca5 platform=linux arch=amd64
```

此输出格式用于外部工具（如 admin 包 probe、自更新逻辑）探测当前运行的二进制信息。

---

## 3. 总体流程

```
编译时（本地/CI）                    上传时（浏览器 → Admin）
  │                                  │
  │ 1. 编译二进制                     │ 1. 选择 .pkg 文件
  │ 2. 生成 meta.json                │ 2. POST /api/packages/upload
  │ 3. 用私钥签名                     │ ──────────────────────────►
  │ 4. 打包成 .pkg                   │    解压 → 验证签名 → 提取元数据
  │                                  │ ◄──── 201 {package entry} ────
  │                                  │
  │                                  │ 3. 文件列表显示
  │                                  │    版本 / 平台 / 架构 / 大小 / 签名状态 / 发布状态
  │                                  │
  │                                  │ 4. 标记已发布（可选）
  │                                  │    POST /api/packages/{id}/publish
  │                                  │    → 只改状态，不替换本地程序
```

---

## 4. API 设计

所有端点走现有 `authMiddleware`（登录 session / token）。

### 4.1 GET /api/packages — 文件列表

```json
{
  "packages": [
    {
      "id": "uuid",
      "filename": "phaethon-v1.0-linux-amd64.pkg",
      "size": 24576000,
      "uploadedAt": "2026-09-22T10:00:00Z",
      "signatureValid": true,
      "meta": {
        "version": "v1.0.0",
        "platform": "linux",
        "arch": "amd64",
        "buildTime": "2026-09-22T09:50:00Z",
        "gitCommit": "abc123def",
        "goVersion": "go1.21.0"
      },
      "published": false
    }
  ]
}
```

- `signatureValid`：签名验证结果（上传时验证，存储结果）
- `published`：是否标记为"已发布"（当前活跃版本）
- 按上传时间倒序

### 4.2 POST /api/packages/upload — 上传

- Body：`multipart/form-data`，字段 `file`（.pkg 文件）
- 服务端流程：
  1. 接收文件，保存到临时位置
  2. 解压 zip → 得到 binary、meta.json、signature
  3. 验证签名：
     - 计算 `hash = sha256(binary + meta.json)`
     - 用内置公钥验证 signature
     - 失败 → 删除临时文件，返回 400 `{"error": "signature verification failed"}`
  4. 签名通过 → 解析 meta.json → 存储到 `.phaethon/packages/{uuid}.pkg`（保留原始 .pkg）
  5. 写元数据到 `.phaethon/packages/{uuid}.json`（包含 meta、signatureValid、uploadedAt）
  6. 返回 201 `{"id": "uuid", ...}`
- 大小上限：默认 100MB（`admin.package-upload-max-mb` 可配）

### 4.3 GET /api/packages/{id}/download — 下载

- 返回原始 .pkg 文件
- Content-Type: `application/octet-stream`
- Content-Disposition: `attachment; filename="phaethon-v1.0-linux-amd64.pkg"`

### 4.4 POST /api/packages/{id}/publish — 标记已发布

- Body：`{}`（空）
- 流程：
  1. 将指定文件标记为 `published: true`
  2. 其他文件的 `published` 自动设为 `false`（单文件发布）
  3. 返回 200 `{"status": "published", "id": "..."}`
- **不触发本地自更新**（只是改状态标记）

### 4.5 DELETE /api/packages/{id} — 删除

- 删除 `.pkg` + `.json`
- 返回 200 `{"status": "deleted"}`

---

## 5. 安全设计

| # | 威胁 | 对策 |
|---|------|------|
| 1 | 无认证节点被任意上传 | **Ed25519 签名验证**：只有持有私钥的发布者能创建有效包，签名同时保证来源可信 + 完整性。无需额外的认证门控（签名即信任） |
| 2 | 上传超大文件耗尽磁盘 | 大小上限（默认 100MB）+ `MaxBytesReader` |
| 3 | 传输损坏 / 篡改 | **Ed25519 签名验证**：签名覆盖 binary + meta.json，任何篡改导致验证失败 |
| 4 | 恶意二进制 | **不执行上传的二进制**（无 probe）；元数据从签名覆盖的 meta.json 读取 |
| 5 | 路径穿越 | 服务端生成 UUID 作为文件名；zip 解压时校验路径 |
| 6 | CSRF | 与现有 API 同基线：SameSite session cookie / token 认证 |

---

## 6. UI 设计（/package 页面）

导航项「📦 发布包」。

布局：

1. **上传区**：拖拽 + 文件选择；上传进度条；完成后刷新列表
2. **文件列表**：文件名 / 版本 / 平台 / 架构 / 大小 / 上传时间 / 签名状态（✅ / ❌）/ 发布状态（🏆 已发布 / 未发布）/ 操作（下载、标记发布、删除）
3. **发布确认弹窗**：「标记为已发布？这只是状态标记，不会替换本地程序」

i18n：`package.*` 命名空间，zh/en 全套。

---

## 7. 存储结构

```
.phaethon/
└── packages/
    ├── {uuid}.pkg       (原始 .pkg 文件)
    └── {uuid}.json      (元数据)
```

`{uuid}.json` 结构：

```json
{
  "id": "uuid",
  "filename": "phaethon-v1.0-linux-amd64.pkg",
  "size": 24576000,
  "uploadedAt": "2026-09-22T10:00:00Z",
  "signatureValid": true,
  "meta": {
    "version": "v1.0.0",
    "platform": "linux",
    "arch": "amd64",
    "buildTime": "2026-09-22T09:50:00Z",
    "gitCommit": "abc123def",
    "goVersion": "go1.21.0"
  },
  "published": false
}
```

---

## 8. 变更文件清单

| 文件 | 变更 |
|------|------|
| `docs/plans/admin_package_upload.md` | 本文档（v0.3.0 重大简化） |
| `admin/package.go`（重写） | 简化为文件管理（上传/列表/下载/删除/标记发布）；移除 probe、自更新、缓存区逻辑；新增 .pkg 解压 + Ed25519 签名验证 |
| `admin/admin.go` | 路由调整：`/api/packages`（复数）；移除 `/api/package/staged/`、`/api/package/publish`（自更新）、`/api/package/cache-hash` |
| `admin/templates/package.html`（重写） | 简化 UI：上传 + 文件列表（无缓存区、无 probe 徽章、无发布确认哈希） |
| `admin/static/i18n.js` | 更新 `package.*` 翻译（移除 probe、自更新相关） |
| `config/config.go` | `AdminConfig` 保留 `package-upload-max-mb`（默认 100）、`package-upload-insecure`（默认 false） |
| `pkg/signing/signing.go`（新增） | Ed25519 签名/验证工具；内置公钥（或从配置读取） |

---

## 9. 决策记录（2026-09-22 简化评审）

| # | 决策点 | 结论 |
|---|--------|------|
| D1 | 打包格式 | `.pkg`（zip：binary + meta.json + signature），单文件原子性 |
| D2 | 签名算法 | Ed25519（现代、快、安全） |
| D3 | 完整性验证 | 签名覆盖 binary + meta.json，不单独算 sha256 |
| D4 | 元数据提取 | 从签名覆盖的 meta.json 读取，**不执行二进制**（无 probe） |
| D5 | 发布语义 | 仅标记状态，**不触发本地自更新**（自更新是独立议题） |
| D6 | 缓存区 | 移除（p2p-cache UI 不再显示） |
| D7 | 文件数量限制 | 不限制（由磁盘空间自然限制） |

---

## 10. 验收标准

1. 上传有效 .pkg（签名正确）→ 201，文件列表可见，元数据正确（版本/平台/架构）
2. 上传篡改的 .pkg（签名无效）→ 400，无文件残留
3. 下载 .pkg → 文件完整，可重新上传
4. 标记已发布 → 该文件 `published: true`，其他文件 `published: false`
5. 删除文件 → 200，列表不再显示
6. MS9（无认证、未开 insecure）→ 上传接口 403
7. 上传超大文件（>100MB）→ 413
8. 签名验证失败 → 400，错误信息明确

---

## 11. 后续议题（本期不实现）

1. **本地自更新**：基于"已发布"标记，下载并替换本地二进制（独立设计）
2. **跨节点分发**：节点间同步"已发布"文件（基于 mesh 网络）
3. **编译命令**：`phaethon build --sign-key` 生成 .pkg
4. **密钥管理**：私钥存储、公钥轮换机制
5. **版本历史**：保留多个版本，支持切换"已发布"标记
