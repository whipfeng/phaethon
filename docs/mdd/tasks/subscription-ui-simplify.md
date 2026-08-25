# subscription-ui-simplify.md

## 任务信息

- 分支名：`subscription-ui-simplify`
- 目标：简化订阅/代理组 Admin UI，使代理组平铺展示手动+订阅节点，并提供统一测速入口
- 创建日期：2026-07-14

## 阶段 1: 文档与准备

### Task 1.1: 完成 MDD 文档
- [x] 创建 `docs/index.md`
- [x] 创建 `docs/specs/core_spec.md` v0.1.0
- [x] 创建 `docs/plans/subscription_group_ui_design.md` v0.1.0
- [x] 创建本任务文件

## 阶段 2: 后端数据模型与 API

### Task 2.1: 调整 ProxyGroup 成员逻辑
- [x] 将 `ProxyGroup.activeMembersLocked()` 改为始终返回合并后的 `Members`
- [x] 调整 `rebuildProxiesLocked()` 成员顺序为 `SubMembers` + `ManualMembers`（订阅节点优先，手动兜底）
- [x] 验证 `Next()` 在 manual + subscription 混合场景下按顺序选择
- [x] 确保 `best` / `load-balance` 在成员变化后仍能正确选择
- [x] 运行 `go test ./...` 无回归

### Task 2.2: 新增 group members / test / active-member API
- [x] 在 `admin.go` 注册路由
  - `GET /api/groups/{name}/members`
  - `POST /api/groups/{name}/test`
  - `POST /api/groups/{name}/active-member`
- [x] 实现 `apiGroupMembers`：返回平铺成员 + 健康 + active 标记
- [x] 实现 `apiGroupTest`：并发测试所有成员并返回快照
- [x] 实现 `apiGroupActiveMember`：select 组设置活动成员并持久化
- [x] 实现辅助函数：判断成员来源、计算 active 成员

### Task 2.3: 统一健康检查入口
- [x] 确保 `CheckProxyHealth` 可处理手动成员与订阅成员
- [x] 手动成员（非 DIRECT/REJECT）也参与一次性组测速
- [x] 嵌套组在测速时按 `Next()` 是否非空判定存活
- [x] 测试并提交

## 阶段 3: 前端页面重构

### Task 3.1: 重写 proxy-group 卡片
- [x] 在 `proxies.html` 中用折叠表格替换“Select Nodes”按钮
- [x] 成员表格列：名称、类型、服务器、端口、状态、延迟、active 标记
- [x] 为 `select` 组行绑定点击事件，调用 `setActiveMember`
- [x] 添加组级“测速”按钮，调用 `testGroup`
- [x] 默认只渲染少量行，长列表可展开

### Task 3.2: 简化 group modal
- [x] 保留 Group Modal 作为编辑入口
- [x] 移除 manual/subscription 模式切换 segmented control
- [x] 允许同时配置手动成员与订阅源
- [x] 保存时正确组装 `proxies` / `subscription` / `subscription-filter`
- [x] 保留“高级筛选”小按钮，用于打开 NodeViewer

### Task 3.3: 简化 subscriptions 页面
- [x] 删除 `viewSubNodes` 按钮与单节点测速逻辑
- [x] 保留 refresh / edit / delete / toggle
- [x] 清理内联 script 中不再使用的函数

### Task 3.4: 调整 NodeViewer
- [x] 将 `NodeViewer` 从默认入口改为“高级筛选”弹窗
- [x] 弹窗保存后刷新成员表格

## 阶段 4: 验证与收尾

### Task 4.1: 本地构建与分组行为验证
- [x] `cd phaethon && go test ./...`
- [x] `cd phaethon && go build`
- [x] 启动本地实例，创建 select/best/load-balance 各一个组
- [x] 验证平铺列表、测速、active 切换、配置保存

### Task 4.2: 更新索引、收尾并提交
- [x] 更新 `docs/index.md` 版本号（如有变更）
- [x] 标记所有 task 为 [x]
- [x] 每个 task 独立 commit
- [x] 最终 push 分支（当前在 master，已 push）
