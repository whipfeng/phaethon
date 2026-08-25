# phaethon MDD 文档说明

MDD（Markdown-Driven Development）用于在编码前固化需求、接口与验收标准，避免在实现过程中反复返工。

## 目录结构

```
docs/mdd/
├── README.md                          # 本文件
├── index.md                           # 文档索引与版本清单
├── glossary.md                        # 项目术语表
├── specs/                             # 规格文档：数据模型、API、行为规则
│   └── core_spec.md
├── plans/                             # 设计方案：背景、架构、决策、风险
│   └── subscription_group_refactor.md
├── tasks/                             # 开发任务：可执行的 checklist
│   └── subscription-group-refactor.md
└── templates/                         # 新建 spec/plan/task 的模板
    ├── spec_template.md
    ├── plan_template.md
    └── task_template.md
```

## 文档类型说明

| 类型 | 解决的问题 | 阅读对象 | 稳定后是否常改 |
|------|-----------|---------|--------------|
| Spec | 数据模型、API 格式、业务规则是什么 | 前后端开发、测试 | 少改 |
| Plan | 为什么做、怎么做、关键决策与风险是什么 | 开发、 reviewer | 随需求调整 |
| Task | 具体做什么、做到什么程度算完成 | 开发、PM | 执行时更新 |

## 使用流程

1. **需求澄清**：先写或更新 Spec，确认数据结构与接口。
2. **方案设计**：写 Plan，描述架构、决策、风险、验收标准。
3. **任务拆解**：写 Task，按阶段列出 checklist。
4. **开发**：每完成一个 task 子项就勾选，保持文档与代码同步。
5. **验收**：Task 全部勾选、Plan 验收标准通过、Spec 无歧义后提交。

## 模板使用

新增功能时，从 `templates/` 复制对应模板到 `specs/` / `plans/` / `tasks/`，按提示填充。完成后在 `index.md` 中登记。

## 版本约定

- Spec 与 Plan 使用语义化版本 `v{major}.{minor}.{patch}`。
- 不兼容的接口或行为变更升 major；新增字段/功能升 minor；修正笔误/示例升 patch。
- Task 文件不需要版本号，但应记录创建日期与依赖的 Plan。
