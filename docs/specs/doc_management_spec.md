> 版本: 1.1.0
> 日期: 2026-08-27
> 状态: ACTIVE
> 负责人: Phaethon Dev

# 文档管理规范 (Document Management Specification)

## 1. 目录结构

```
docs/
├── inputs/        # 原始需求文档（用户提供的、外部输入的、会议纪要、故障分析等）
├── specs/         # 规格定义（API 规格、数据模型、行为规则、SLA 指标）
├── plans/         # 架构设计（分层架构、时序图、状态机、关键决策 ADR）
├── tasks/         # 已完成任务的归档（与 plan 对应，按分支或模块命名）
├── templates/     # spec / plan / task 模板
├── runbooks/      # 运维经验（部署踩坑、故障处理、性能调优）
├── glossary.md    # 项目术语表
└── index.md       # 本文档（文档索引与管理规范）
```

## 2. 各目录职责

### 2.1 `docs/inputs/` — 原始需求

- **用途**: 存放所有未经加工的原始输入材料。
- **内容**: 用户需求描述、会议纪要、竞品分析、参考截图、外部文档、历史故障分析等。
- **命名规则**: 推荐 `{日期}_{简述}.md` 或保留原始文件名，如 `20260709_initial_requirements.md`。
- **原则**: 只增不改，保留原始面貌，作为需求的溯源依据。

### 2.2 `docs/specs/` — 规格定义

- **用途**: 从原始需求中提炼的、经过评审的正式规格文档。
- **内容**: API I/O 定义、数据模型 JSON Schema、业务规则、技术约束。
- **命名规则**: `{模块名}_spec.md`，如 `core_spec.md`、`auth_spec.md`。
- **原则**: 冻结后变更需走变更控制（版本号递增 + Review）。

### 2.3 `docs/plans/` — 架构设计

- **用途**: 基于 spec 的技术实现方案。
- **内容**: 分层架构图、时序图 (Mermaid)、状态机、并发控制、降级策略、关键 ADR。
- **命名规则**: `{模块名}_design.md`，如 `core_design.md`、`tun_design.md`。
- **原则**: 每个重要设计决策需记录 ADR (Architecture Decision Record)，可直接嵌入在 design 文档的“关键设计决策”章节中。

### 2.4 `docs/tasks/` — 开发任务

- **用途**: 功能分支或模块的开发任务工单，与 plan 对应。
- **内容**: 阶段 + Task 列表，含验收标准和完成状态。
- **命名规则**: `{模块名}-tasks.md` 或 `{分支名}.md`，如 `tun-tasks.md`。
- **原则**: 从任务创建起就在本目录编辑，完成后保留，无需迁移。

### 2.5 `docs/runbooks/` — 运维经验

- **用途**: 沉淀部署、故障排查、性能调优经验。
- **内容**: 部署踩坑记录、故障处理步骤、性能优化案例。
- **命名规则**: `{主题}.md`，如 `deployment-lessons.md`。
- **原则**: 问题解决后即归档，避免重复踩坑。

### 2.6 `docs/templates/` — 文档模板

- **用途**: 新建 spec / plan / task 时复用。
- **内容**: `spec_template.md`、`plan_template.md`、`task_template.md`。

## 3. 文档生命周期

```
inputs/ (原始输入)
   │
   ▼  提炼 & 评审
specs/ (规格冻结)
   │
   ▼  设计 & 评审
plans/ (架构方案，含 ADR)
   │
   ▼  拆解
tasks/{模块名}-tasks.md (原子化工单)
   │
   ▼  实现
src/ (生产代码)
```

## 4. 项目运行时产物目录

除 `docs/` 外，项目根目录还维护以下运行时/测试产物目录。这些目录均加入 `.gitignore`，不进入版本库，但必须按规范存放，避免散落在项目根目录。

### 4.1 `bin/` — 编译产物

- **用途**: 存放本地编译生成的可执行文件、临时二进制及备份副本（如 `phaethon.exe~`）。
- **约束**:
  - 所有平台产物统一放此处，禁止在根目录保留 `phaethon`、`phaethon.exe`、`phaethon-test*` 等文件。
  - 该目录可随时 `rm -rf bin/` 后重新构建，不应存放源代码或配置。
  - 产物命名建议：`phaethon`（正式版）、`phaethon-test`（测试版）、`phaethon-watchdog`（看门狗）。

### 4.2 `logs/` — 运行时日志

- **用途**: 存放进程运行期间产生的 `.log`、`.err`、`.out`、`.err.log`、`.out.log` 等日志文件。
- **约束**:
  - 所有运行时日志统一放此处，禁止在根目录保留 `py-ssh.err`、`phaethon-watchdog.log` 等文件。
  - 日志文件可按模块/日期命名，如 `tun-run-20260827.err.log`。
  - 该目录可随时清理，不纳入版本控制。

### 4.3 `tests/e2e/tun/` — TUN 端到端测试目录

- **用途**: 存放 TUN 模式的端到端测试配置、脚本、抓包、运行时产物等本地测试资料。
- **约束**:
  - 该目录整体不纳入版本控制。
  - 内部可按场景划分子目录，如 `tests/e2e/tun/conf-tun-real-test/`。
  - 大体积抓包文件（`.pcapng`）建议放在该目录下，但禁止提交到 Git；如需共享，使用外部存储或 LFS。
  - 可复用的测试配置示例可单独归档到 `tests/fixtures/`（不忽略）或 `docs/inputs/`。

## 5. 版本控制规则

| 变更类型 | 版本号 | 示例 |
|---------|--------|------|
| 初始草稿 | 0.x.x | 0.1.0 |
| 评审通过 | 1.0.0 | 首次正式发布 |
| 向后兼容的补充 | x.1.0 | 新增非核心字段 |
| 破坏性变更 | (x+1).0.0 | 修改核心 API 契约 |

- Spec 与 Plan 使用语义化版本 `v{major}.{minor}.{patch}` 或 `{major}.{minor}.{patch}`。
- 不兼容的接口或行为变更升 major；新增字段/功能升 minor；修正笔误/示例升 patch。
- Task 文件不需要版本号，但应记录创建日期与依赖的 Plan。

## 6. 文档模板

每个 spec / plan 文件必须包含头部元信息：

```markdown
# {文档标题}

> 版本: x.y.z
> 日期: YYYY-MM-DD
> 状态: DRAFT | ACTIVE | DEPRECATED
> 负责人: {姓名}

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-08-26 | 初始版本 | xxx |
```

## 7. 评审流程

1. 作者提交变更，必要时指定 Reviewer。
2. 至少 1 人 Review 通过后方可合并（破坏性变更需更严格）。
3. 合并后更新 `docs/index.md` 索引。
4. 破坏性变更需在提交说明中注明影响范围。

## 8. 其他项目级文档

| 文件/目录 | 用途 |
|-----------|------|
| `README.md` | 项目简介、快速启动指南 |
| `docs/glossary.md` | 项目术语表 |
| `docs/index.md` | 文档索引入口 |
| `docs/templates/` | 文档模板 |

## 9. 历史迁移说明

- 2026-08-27：新增项目根目录运行时产物规范，将散落在根目录的编译产物（`phaethon*`）、日志（`py-ssh.err`、`*.log`）和 TUN 测试目录（`conf-tun-*`）分别归集到 `bin/`、`logs/`、`tests/e2e/tun/`，并更新 `.gitignore`。
- 2026-08-26：将 `docs/mdd/` 多层目录结构扁平化为 `docs/` 下 `inputs/`、`specs/`、`plans/`、`tasks/`、`runbooks/`、`templates/`。
- ADR 文件不再单独存放于 `decisions/`，而是合并到对应 `plans/` 文档的“关键设计决策”章节。
- 散落于 `docs/` 根目录的历史设计文档（反向连接相关）已归并到 `plans/` 或 `inputs/`。
