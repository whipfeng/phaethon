# Docs - phaethon 文档中心

> 本目录是项目所有文档的唯一归口，遵循 MDD (Markdown-Driven Development) 体系。
> 文档管理规范详见 [specs/doc_management_spec.md](specs/doc_management_spec.md)。

## 目录结构

| 目录 | 用途 | 内容示例 |
|------|------|----------|
| `inputs/` | 原始需求与历史分析 | 用户需求、会议纪要、故障分析、历史设计草稿 |
| `specs/` | 规格定义 | API 规格、数据模型、行为规则、文档管理规范 |
| `plans/` | 架构设计 | 分层架构图、时序图、状态机、关键决策 ADR |
| `tasks/` | 任务归档 | 已完成分支/模块的开发任务记录 |
| `runbooks/` | 运维经验 | 部署踩坑、故障处理、性能调优 |
| `templates/` | 文档模板 | spec / plan / task 模板 |
| `glossary.md` | 术语表 | 项目术语与缩写 |

## 文档索引

### specs/ (规格定义)

| 文档 | 描述 | 版本 | 状态 |
|------|------|------|------|
| [doc_management_spec.md](specs/doc_management_spec.md) | 文档管理规范 | 1.0.0 | ACTIVE |
| [core_spec.md](specs/core_spec.md) | 核心数据模型、API 规格与业务规则 | v0.2.0 | DRAFT |
| [admin_spec.md](specs/admin_spec.md) | Admin 面板页面与完整 API 总览 | v0.1.0 | DRAFT |
| [protocol_spec.md](specs/protocol_spec.md) | 入站/出站协议支持矩阵与实现约定 | v0.1.0 | DRAFT |
| [reverse_spec.md](specs/reverse_spec.md) | 反向连接、Registry 与统一帧协议规格 | v0.1.0 | DRAFT |
| [tun_spec.md](specs/tun_spec.md) | TUN 模式架构、路由与 DNS 规则 | v0.3.0 | DRAFT |
| [config_spec.md](specs/config_spec.md) | 配置格式、加载顺序与环境变量规则 | 1.0.0 | ACTIVE |

### plans/ (架构设计)

| 文档 | 描述 | 版本 | 状态 |
|------|------|------|------|
| [extract_design.md](plans/extract_design.md) | 项目提取与配置重构设计 | 1.0.0 | ACTIVE |
| [subscription_group_refactor.md](plans/subscription_group_refactor.md) | 订阅组概念重构：移除 static/dynamic，解耦 membership 与 active | v0.2.1 | DRAFT |
| [subscription_group_ui_design.md](plans/subscription_group_ui_design.md) | 订阅/代理组 UI 简化设计 | v0.1.2 | DRAFT |
| [tun_design.md](plans/tun_design.md) | TUN 模式实现：问题修复、路由决策与 Proxifier 方案对比 | v0.1.0 | DRAFT |
| [tun_watchdog_design.md](plans/tun_watchdog_design.md) | 统一跨平台 TUN 实现 + 引擎级看门狗 | v0.1.0 | DRAFT |
| [ssh_design.md](plans/ssh_design.md) | SSH 代理改进计划 | v0.1.0 | DRAFT |
| [reverse_control_channel_design.md](plans/reverse_control_channel_design.md) | 动态反向控制通道设计 | - | DRAFT |
| [reverse_udp_design.md](plans/reverse_udp_design.md) | 反向 UDP 通道设计 | - | DRAFT |
| [unified_frame_protocol_design.md](plans/unified_frame_protocol_design.md) | 统一反向连接帧协议设计 | - | DRAFT |
| [tun_stability_design.md](plans/tun_stability_design.md) | TUN 稳定性修复：竞态、goroutine 泄漏、Fake-IP 清理 | v0.1.0 | DRAFT |
| [tun_route_and_fakeip_fixes.md](plans/tun_route_and_fakeip_fixes.md) | TUN 路由清理顺序与 Fake-IP 注册/释放一致性修复 | v0.1.0 | DRAFT |

### tasks/ (任务归档)

| 文档 | 描述 | 分支/模块 |
|------|------|----------|
| [extract-init.md](tasks/extract-init.md) | 提取与配置重构任务 | extract-init |
| [subscription-group-refactor.md](tasks/subscription-group-refactor.md) | 订阅组概念重构 | subscription-group-refactor |
| [subscription-ui-simplify.md](tasks/subscription-ui-simplify.md) | 订阅/代理组 UI 简化 | subscription-ui-simplify |
| [tun-tasks.md](tasks/tun-tasks.md) | TUN 问题修复与路由优化 | master |
| [tun-watchdog-tasks.md](tasks/tun-watchdog-tasks.md) | 跨平台 TUN + 引擎看门狗 | master |
| [ssh-tasks.md](tasks/ssh-tasks.md) | SSH 代理改进 | master |
| [tun-stability-tasks.md](tasks/tun-stability-tasks.md) | TUN 稳定性修复 | master |
| [tun-route-fakeip-fixes.md](tasks/tun-route-fakeip-fixes.md) | TUN 路由与 Fake-IP 修复 | master |

### inputs/ (原始需求与历史分析)

| 文档 | 描述 |
|------|------|
| [reverse-udp-topology-bug-analysis.md](inputs/reverse-udp-topology-bug-analysis.md) | 反向 UDP 拓扑 Bug 分析 |
| [reverse-udp-topology-correct.md](inputs/reverse-udp-topology-correct.md) | 反向 UDP 正确拓扑记录 |

### runbooks/ (运维经验)

暂无。问题解决后请归档至此。

### templates/ (文档模板)

| 文档 | 用途 |
|------|------|
| [spec_template.md](templates/spec_template.md) | Spec 模板 |
| [plan_template.md](templates/plan_template.md) | Plan 模板 |
| [task_template.md](templates/task_template.md) | Task 模板 |

### 术语表

| 文档 | 描述 |
|------|------|
| [glossary.md](glossary.md) | 项目术语表 |

## 核心原则

1. **单向流动**: `inputs → specs → plans → tasks → code`
2. **只增不改**: `inputs/` 保留原始面貌，不修改。
3. **版本控制**: spec/plan 变更需版本号递增 + Review。
4. **ADR 嵌入 plan**: 关键架构决策直接写在对应 design 文档中，不再单独维护 `decisions/` 目录。
5. **经验沉淀**: 运维问题解决了就归档到 `runbooks/`，避免重复踩坑。
