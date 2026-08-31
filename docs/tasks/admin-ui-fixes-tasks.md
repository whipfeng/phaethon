# Admin UI 修复任务

> 创建日期: 2026-09-01
> 关联输入: docs/inputs/20260901_admin_ui_issues.md
> 状态: IN PROGRESS

## Task 1: Dashboard TUN 卡片展示完整信息

- [x] 添加 Device 行展示 `deviceName`
- [x] 添加 Counters 区块展示 `stats.readPackets`、`stats.writePackets`
- [x] 添加 Fake-IP 统计展示 `stats.fakeIP.*`
- [x] 添加 Probe URLs 区块展示 `probeURLs`
- [x] 更新 `renderTUN()` 函数填充新字段

## Task 2: 移除废弃的 base/env 切换功能

- [x] 移除 layout.html 侧边栏 `config-target` 区块
- [x] 移除 dashboard.html `Save Target` 行、`Env` 行、`env-hint`
- [x] 移除 app.js `switchSaveTarget()` 函数
- [ ] 清理后端 `saveTarget`、`envPath`、`envConf` 相关逻辑
- [ ] 移除 `/api/config/target` API 端点

## Task 3: TUN 开关状态和动作对齐

- [ ] 开关 `checked` 状态改为基于 `data.running`（实际运行状态）
- [ ] 开关动作添加 loading 状态（禁用开关 + spinner）
- [ ] 开关失败时恢复原始状态并显示错误提示
- [ ] 如果 `enabled=true` 但 `running=false`，显示警告标识

## Task 4: Rules 页面拖动排序

- [x] 添加拖动列（drag handle ⠿）
- [x] 实现 HTML5 drag-and-drop 事件处理
- [x] 添加 drop indicator 视觉反馈
- [x] 添加相关 CSS 样式
- [x] 保留上下箭头按钮作为备选
