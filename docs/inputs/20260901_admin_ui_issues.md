# Admin UI 待修复问题汇总

> 日期: 2026-09-01
> 来源: 开发过程中发现的 UI 问题
> 状态: ACTIVE

## 1. Dashboard TUN 卡片 — API 返回但 UI 未展示

API `/api/tun` 返回了以下字段，但 Dashboard TUN 卡片未展示：

| 字段 | 说明 | 状态 |
|------|------|------|
| `deviceName` | 设备名称（PhaethonTUN） | 已修复：新增 Device 行 |
| `probeURLs` | 探测 URL 列表 | 已修复：新增 Probe URLs 区块 |
| `stats.readPackets` | 读包数 | 已修复：新增 Counters 区块 |
| `stats.writePackets` | 写包数 | 已修复 |
| `stats.fakeIP.domainCount` | Fake-IP 域名数 | 已修复 |
| `stats.fakeIP.registeredCount` | 已注册 Fake-IP 数 | 已修复 |
| `stats.fakeIP.realIPCacheCount` | 真实 IP 缓存数 | 已修复 |

## 2. 废弃的 base/env 配置切换功能（应移除）

以下功能已废弃，应移除：

- [x] 侧边栏 `CONFIG BASE/ENV` 切换按钮（`switchSaveTarget()`）— 已移除
- [x] Dashboard `Save Target` 行和 `Env` 行 — 已移除
- [x] `runtime = base + <env>` 提示 — 已移除
- [ ] 后端 `saveTarget`、`envPath`、`envConf` 逻辑 — 待清理
- [ ] `/api/config/target` API 端点 — 待清理

## 3. TUN 模式开关状态和动作未对齐

**问题描述：**
- TUN toggle 开关的 `checked` 状态基于 `data.enabled`（配置是否启用），但实际运行状态是 `data.running`
- 当 TUN 配置为启用但实际未运行（启动失败、权限不足等），开关显示为开启但实际未工作
- 用户操作开关后，UI 没有即时反馈，依赖 SSE 刷新

**期望行为：**
- 开关状态应反映实际运行状态（running），而非配置状态（enabled）
- 开关动作应有即时 UI 反馈（loading 状态）
- 如果启用但运行失败，应有错误提示

**状态：** 待修复

## 4. Rules 页面支持拖动排序

**需求：** 规则记录应该可以拖动到一个位置插入

**实现：**
- [x] 添加拖动列（drag handle）
- [x] 实现 HTML5 drag-and-drop
- [x] 添加视觉反馈（drop indicator line）
- [x] 保留上下箭头按钮作为备选

**状态：** 已实现
