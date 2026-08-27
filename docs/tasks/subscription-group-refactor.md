# subscription-group-refactor.md

## 任务信息

- 分支名：`subscription-group-refactor`
- 目标：重构订阅组概念，移除 static/dynamic 订阅模式，解耦 membership 与 active
- 创建日期：2026-07-14
- 依赖计划：[subscription_group_refactor.md](../plans/subscription_group_refactor.md)

## 阶段 1: 文档与准备

### Task 1.1: 更新核心规格
- [x] 更新 `docs/mdd/plans/subscription_group_refactor.md` v0.2.0
- [x] 更新 `docs/mdd/specs/core_spec.md` v0.2.0（移除 subscription-mode，调整 active-member 语义）
- [x] 更新 `docs/mdd/index.md` 版本号
- [x] 创建本任务文件

## 阶段 2: 后端数据模型重构

### Task 2.1: 调整 ProxyGroup 字段与语义
- [x] 从 `ProxyGroup` 中移除 `SubscriptionMode` 使用（YAML/JSON 保留读取但不再参与逻辑）
- [x] 将 `SubscriptionSelected` 语义改为“select 组的 active 节点名列表/单个节点”
- [x] 新增/复用 `ActiveMember` 概念，用于 `select` 组持久化当前活动成员
- [x] 更新 `rebuildProxiesLocked()`：`SubMembers` = 全部匹配 filter 的订阅候选节点

### Task 2.2: 更新选择逻辑
- [x] `activeMembersLocked()` 保持返回完整 `Members`
- [x] `Next()` / `PickActiveMember()` 按 group type 计算 active
- [x] `select` 组持久化 active 回退逻辑：若 active 不在当前 Members 中，回退到第一个成员

### Task 2.3: 配置加载与迁移
- [x] 在 `Init()` 或加载阶段忽略 `subscription-mode`
- [x] 旧 `subscription-selected` 迁移：取第一个有效节点作为 `active-member`，其余清空
- [x] 确保旧配置加载不 panic、不丢失手动成员

### Task 2.4: 调整 Admin API
- [x] `apiGroupMembers`：返回全部成员，`selected` = 是否属于本组，`active` = 当前生效
- [x] `apiGroupActiveMember`：仅修改 active 指针/顺序，不修改 membership
- [x] `apiGroupSubscription` / `apiGroupSubscriptionUpdate`：改为只读/管理 filter，不再写入 membership
- [x] `apiGroups` / `saveGroup`：移除 `subscription-mode` 字段处理

### Task 2.5: 测试后端
- [x] 更新 `config` 包单元测试
- [x] 新增/更新 active-member 与 membership 相关测试
- [x] `go test ./...` 通过

## 阶段 3: 前端交互调整

### Task 3.1: 简化 Group Modal
- [x] 保留 Group Modal 作为编辑入口（不改为内联编辑）
- [x] 移除 static/dynamic 模式切换按钮
- [x] 保留订阅源选择、filter 输入、手动成员 picker
- [x] 清理 i18n 中 static/dynamic / manual-subscription 模式等废弃文案
- [x] “高级筛选”按钮文案改为“查看订阅节点”或类似（当前版本已移除该按钮，通过成员表直接交互）

### Task 3.2: 调整成员表格交互
- [x] `select` 组成员行点击调用 `setActiveMember`，仅切换 active 高亮
- [x] `best` / `load-balance` 组行不响应点击 active
- [x] 成员表格默认展示全部成员，长列表折叠

### Task 3.3: 调整 NodeViewer
- [x] NodeViewer 打开时显示订阅全部候选与当前 filter 效果
- [x] 保存时只更新 `subscription-filter`，不修改 membership
- [x] 移除 `subscription-selected` 相关前端状态

### Task 3.4: 前端测试
- [x] 本地启动实例验证 UI 行为
- [x] 检查控制台无报错

## 阶段 4: 验证与收尾

### Task 4.1: 本地验证
- [x] `go test ./...` 无回归
- [x] `go build` 成功
- [x] 本地创建 select/best/load-balance 混合订阅组，验证 active 切换与测速

### Task 4.2: 远程验证
- [x] 编译并部署到 `<registry-host>`
- [x] 使用 `test-sub` 验证成员表、active 切换、订阅刷新、NodeViewer

### Task 4.3: 更新索引并收尾
- [x] 更新 `docs/mdd/index.md`
- [x] 标记所有 task 为 [x]
- [x] 每个 task 独立 commit（如用户需要）
- [x] 最终 push 分支（当前在 master，已 push）
