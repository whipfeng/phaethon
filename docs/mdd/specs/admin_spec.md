# admin_spec.md

## 元数据

- 文档类型：Spec
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-07-14

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-07-14 | 初始版本：Admin 面板页面与 API 总览 | Claude |

## 1. 概述

Admin 是 phaethon 内置的 Web 管理面板与 HTTP API，默认监听 `:39999`。它提供配置管理、实时状态、反向连接向导、TUN 开关、认证授权等功能。

## 2. 页面路由

| 路径 | 模板 | 说明 |
|------|------|------|
| `/` | dashboard | 首页/统计 |
| `/proxies` | proxies.html | 代理与代理组管理 |
| `/subscriptions` | subscriptions.html | 订阅源管理 |
| `/rules` | rules.html | 路由规则管理 |
| `/mappings` | mappings.html | 入站端口映射管理 |
| `/reverse` | reverse.html | 反向连接向导 |
| `/config` | config.html | 原始配置编辑器 |
| `/login` | login.html | 登录页（启用认证时） |
| `/setup` | setup.html | 首次设置管理员账号 |

## 3. 认证与授权

- 通过配置 `admin.auth-enabled` 开启认证。
- 登录成功后返回 `X-Admin-Token` Cookie/Header，后续请求携带该 token。
- 未认证访问 API 返回 401；访问页面重定向到 `/login`。
- 首次启动若启用认证且无用户，重定向到 `/setup`。

## 4. API 分组

### 4.1 统计与状态

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/stats` | 运行统计 |
| GET | `/api/health` | 健康状态 |
| GET | `/api/events` | SSE 事件流 |

### 4.2 配置管理

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/api/config` | 获取/保存完整配置 |
| GET | `/api/config/raw` | 原始 YAML |
| POST | `/api/config/reset` | 重置配置 |
| POST | `/api/config/reload` | 从磁盘重新加载 |
| GET | `/api/config/target` | 当前保存目标（base/env） |

### 4.3 代理与代理组

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/api/proxies` | 代理列表/创建/更新 |
| GET/DELETE | `/api/proxies/{name}` | 获取/删除代理 |
| POST | `/api/proxies/health-check/{name}` | 单代理健康检查 |
| GET/POST | `/api/groups` | 代理组列表/创建/更新 |
| GET/DELETE | `/api/groups/{name}` | 获取/删除代理组 |
| GET | `/api/groups/{name}/members` | 平铺成员列表 |
| POST | `/api/groups/{name}/test` | 组级测速 |
| POST | `/api/groups/{name}/active-member` | 设置活动成员（select 组） |
| GET/PUT | `/api/groups/{name}/subscription` | 订阅候选节点与 filter |
| POST | `/api/groups/{name}/health-check/{node}` | 单成员健康检查 |

代理组详细数据模型与选择策略见 [core_spec.md](core_spec.md)。

### 4.4 规则与映射

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/api/rules` | 路由规则列表/更新 |
| GET/DELETE | `/api/rules/{idx}` | 获取/删除单条规则 |
| GET/POST | `/api/mappings` | 入站映射列表/创建/更新 |
| GET/DELETE | `/api/mappings/{name}` | 获取/删除映射 |

### 4.5 订阅

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/api/subscriptions` | 订阅源列表/创建/更新 |
| GET/DELETE | `/api/subscriptions/{name}` | 获取/删除订阅源 |
| POST | `/api/subscriptions/{name}/refresh` | 刷新订阅节点池 |
| PATCH | `/api/subscriptions/{name}/toggle` | 启用/禁用 |

### 4.6 反向连接

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/api/reverse` | 反向连接状态/触发操作 |
| GET/POST | `/api/reverse/bindings` | 绑定列表/创建 |
| GET/DELETE | `/api/reverse/bindings/{id}` | 获取/删除绑定 |
| GET/DELETE | `/api/reverse/{id}` | 获取/删除反向项 |

### 4.7 TUN

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/tun` | TUN 状态 |
| POST | `/api/tun` | 启用/禁用 TUN |

### 4.8 认证

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/login` | 登录 |
| POST | `/api/logout` | 登出 |
| GET | `/api/me` | 当前用户信息 |
| POST | `/api/captcha` | 验证码 |
| GET/POST | `/api/admin/auth` | 认证配置 |
| POST | `/api/setup` | 初始化管理员账号 |

## 5. 通用响应约定

- 成功：返回 JSON 对象或 `{ "status": "ok" }`。
- 错误：返回 `{ "error": "message" }`，HTTP 状态码 4xx/5xx。
- 涉及配置修改的接口保存后会触发 `mergeAndInitLocked` 热重载。

## 6. SSE 事件

`/api/events` 返回 `text/event-stream`，用于前端同步配置保存、健康状态变更、反向连接事件等。

## 7. 相关链接

- [core_spec.md](core_spec.md)
- [reverse_spec.md](reverse_spec.md)
- [tun_spec.md](tun_spec.md)
