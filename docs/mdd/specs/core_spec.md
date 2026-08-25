# core_spec.md

## 元数据

- 文档类型：Spec
- 版本：v0.2.0
- 所属项目：phaethon
- 创建日期：2026-07-14

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-07-14 | 初始版本：定义 ProxyGroup / GroupMember / Subscription 数据模型与 Admin API 规格 | Claude |
| v0.1.1 | 2026-07-14 | 调整 Members 顺序为订阅节点在前、手动成员在后，使手动代理成为真正的兜底 | Claude |
| v0.2.0 | 2026-07-14 | 重构订阅组概念：移除 subscription-mode，membership 由 filter 决定，active 单独持久化 | Claude |

## 1. 概述

本文档定义 `phaethon` 运行时与 Admin UI 共享的核心数据模型、配置持久化规则以及 Admin HTTP API 的输入输出格式。

## 2. 数据模型

### 2.1 ProxyGroup

代理组，规则匹配后的下一跳选择器。

| 字段 | YAML Key | JSON Key | 类型 | 必填 | 说明 |
|------|----------|----------|------|------|------|
| Name | `name` | `name` | string | 是 | 组名，全局唯一 |
| Enabled | `enabled` | `enabled` | *bool | 否 | 省略时默认为 true |
| Type | `type` | `type` | string | 是 | `select` / `best` / `load-balance` |
| Proxies | `proxies` | `proxies` | []string | 条件 | YAML 中保存手动代理/嵌套组名 |
| HealthCheckURL | `health-check-url` | `health-check-url` | string | 否 | 健康检查 URL |
| HealthCheckInterval | `health-check-interval` | `health-check-interval` | *int | 否 | 周期检查间隔（秒），0 表示不自动检查 |
| Subscription | `subscription` | `subscription` | string | 否 | 引用的订阅源名称 |
| SubscriptionFilter | `subscription-filter` | `subscription-filter` | string | 否 | 按节点名过滤订阅候选（正则或子串） |
| SubscriptionSelected | `subscription-selected` | `subscription-selected` | []string | 否 | **已废弃**，加载时迁移到 active-member |
| ActiveMember | `active-member` | `active-member` | string | 否 | `select` 组当前活动成员名；若不在当前 Members 中则回退到第一个成员 |
| SubscriptionMode | `subscription-mode` | `subscription-mode` | string | 否 | **已废弃**，加载时忽略 |

运行时字段（不持久化）：

| 字段 | 类型 | 说明 |
|------|------|------|
| ManualProxies | []string | 从 `Proxies` 复制的手动成员 |
| Members | []GroupMember | 合并后的有序成员：匹配 filter 的订阅节点在前，手动成员在后 |
| ManualMembers | []GroupMember | 手动成员（含嵌套组标记） |
| SubMembers | []GroupMember | 匹配 filter 的订阅节点成员 |
| healthMap | map[string]*healthStatus | 成员健康状态快照 |

### 2.2 GroupMember

组内单个成员，区分来源避免同名冲突。

| 字段 | 类型 | 说明 |
|------|------|------|
| Name | string | 成员名称（代理名或订阅节点名） |
| FromSubscription | bool | true = 来自订阅源；false = 手动代理/嵌套组 |
| IsGroup | bool | true = 手动成员是嵌套代理组 |

健康检查 key：

- 手动成员：`Name`
- 订阅节点：`sub:` + `Name`

### 2.3 Subscription

订阅源，只负责 URL、刷新周期与解析后的节点池。

| 字段 | YAML Key | JSON Key | 类型 | 必填 | 说明 |
|------|----------|----------|------|------|------|
| Name | `name` | `name` | string | 是 | 订阅名 |
| Enabled | `enabled` | `enabled` | *bool | 否 | 省略时默认为 true |
| URL | `url` | `url` | string | 是 | 订阅链接 |
| Interval | `interval` | `interval` | *int | 否 | 自动刷新间隔（秒） |

运行时字段（不持久化）：

| 字段 | 类型 | 说明 |
|------|------|------|
| SubProxies | map[string]*Proxy | 解析后的节点池 |

## 3. 业务规则

### 3.1 组成员构成

1. 手动成员由 `Proxies` 提供，可包含普通代理、DIRECT、REJECT 或嵌套代理组。
2. 若配置了 `Subscription`，则按 `SubscriptionFilter` 过滤后，所有匹配节点都进入 `SubMembers`。
3. 运行时 `Members` = `SubMembers` + `ManualMembers`，订阅节点在前，手动成员在后。
4. `Next()` 选择逻辑统一作用在 `Members` 上，不再区分来源。订阅节点优先参与选择；当所有订阅节点不可用时，手动代理（如 DIRECT）作为兜底。
5. `subscription-mode` 与 `subscription-selected` 已废弃：加载旧配置时忽略 `subscription-mode`，并将旧 `subscription-selected` 的第一个有效节点迁移为 `active-member`。

### 3.2 选择策略

| Group Type | 行为 |
|------------|------|
| `select` | 优先返回 `ActiveMember`；若其不在当前 `Members` 中，则返回第一个成员。 |
| `best` | 返回存活成员中延迟最低者。 |
| `load-balance` | 轮询存活成员。 |

### 3.3 健康检查

- 自动周期检查只对订阅节点执行；手动代理默认标记为存活。
- 手动触发“组测速”时，所有成员（含手动代理、嵌套组、订阅节点）都参与一次性检查。
- `SetHealthImmediate` 用于手动/单点测试，立即覆盖阈值。
- `SetHealth` 用于周期检查，使用连续成功/失败阈值。

## 4. Admin API 规格

### 4.1 通用约定

- 所有 API 返回 JSON。
- 错误响应：`{ "error": "message" }`，HTTP 状态码 4xx/5xx。
- 成功响应视接口而定，通常为对象或 `{ "status": "ok" }`。

### 4.2 订阅 API

#### GET /api/subscriptions

返回订阅源列表（不含节点详情）。

```json
[
  {
    "name": "my-sub",
    "enabled": true,
    "url": "https://example.com/sub",
    "interval": 3600,
    "nodeCount": 12
  }
]
```

#### POST /api/subscriptions

创建/更新订阅源。

请求体：

```json
{
  "name": "my-sub",
  "url": "https://example.com/sub",
  "interval": 3600
}
```

#### DELETE /api/subscriptions/{name}

删除订阅源。若被代理组引用则返回 400。

#### POST /api/subscriptions/{name}/refresh

刷新订阅节点池。

响应：

```json
{
  "status": "ok",
  "nodeCount": 12
}
```

#### PATCH /api/subscriptions/{name}/toggle

启用/禁用订阅源。

请求体：

```json
{ "enabled": false }
```

### 4.3 代理组 API

#### GET /api/groups

返回代理组摘要列表。

```json
[
  {
    "name": "AUTO",
    "enabled": true,
    "type": "select",
    "proxies": ["DIRECT", "vless-443"],
    "manualProxies": ["DIRECT"],
    "subscription": "my-sub",
    "subscription-filter": "",
    "active-member": "vless-443",
    "health-check-url": "http://www.gstatic.com/generate_204",
    "health-check-interval": 300
  }
]
```

#### POST /api/groups

创建/更新代理组。

请求体：

```json
{
  "name": "AUTO",
  "type": "select",
  "proxies": ["DIRECT"],
  "subscription": "my-sub",
  "subscription-filter": "",
  "active-member": "vless-443",
  "health-check-url": "http://www.gstatic.com/generate_204",
  "health-check-interval": 300
}
```

#### DELETE /api/groups/{name}

删除代理组。若被引用则返回 409。

#### PATCH /api/groups/{name}/toggle

启用/禁用代理组。

#### GET /api/groups/{name}/members

返回该组合并后的平铺成员列表，用于 UI 渲染。

```json
[
  {
    "name": "DIRECT",
    "source": "manual",
    "type": "DIRECT",
    "server": "",
    "port": 0,
    "alive": true,
    "latencyMs": 0,
    "lastCheck": "2026-07-14T12:00:00Z",
    "active": false,
    "selected": false
  },
  {
    "name": "vless-443",
    "source": "subscription",
    "type": "vless",
    "server": "1.2.3.4",
    "port": 443,
    "alive": true,
    "latencyMs": 120,
    "lastCheck": "2026-07-14T12:00:00Z",
    "active": true,
    "selected": true
  }
]
```

字段说明：

- `source`: `manual` 或 `subscription`。
- `selected`: 该节点是否属于本组。订阅节点匹配 filter 时为 true；手动成员恒为 true。
- `active`: 按当前 group type 与策略，该节点是否为当前生效成员。

#### POST /api/groups/{name}/test

立即对组内所有成员执行一次健康检查，返回最新健康快照。

响应：

```json
{
  "DIRECT": { "alive": true, "latencyMs": 0, "lastCheck": "..." },
  "vless-443": { "alive": true, "latencyMs": 115, "lastCheck": "..." }
}
```

键规则与 `GroupMember.HealthKey()` 一致：手动成员用名称，订阅节点用 `sub:` 前缀。

#### POST /api/groups/{name}/active-member

仅对 `select` 类型组生效，设置当前活动成员。

请求体：

```json
{
  "name": "vless-443",
  "source": "subscription"
}
```

行为：

- 校验 `name` 是否在当前 `Members` 中；若不存在则返回 400。
- 持久化 `active-member = name`，不修改 `proxies`、`subscription-filter` 或 membership。
- 保存后触发 `RebuildProxies` 与 `triggerReload`。

#### GET /api/groups/{name}/subscription

返回该组引用的订阅源全部候选节点、当前 `filter`、当前 `active-member` 等。

```json
{
  "nodes": [
    { "name": "vless-443", "type": "vless", "server": "1.2.3.4", "port": 443, "alive": true, "latencyMs": 120, "lastCheck": "..." }
  ],
  "active-member": "vless-443",
  "subscription": "my-sub",
  "type": "select",
  "filter": "",
  "healthCheckURL": "http://www.gstatic.com/generate_204",
  "healthCheckInterval": 300
}
```

#### PUT /api/groups/{name}/subscription

修改 `subscription-filter` 与可选的 `active-member`。

请求体：

```json
{
  "filter": "HK|TW|JP",
  "active-member": "vless-443"
}
```

响应：

```json
{
  "filter": "HK|TW|JP",
  "active-member": "vless-443"
}
```

#### POST /api/groups/{name}/health-check

保留。对单个成员执行一次检查：

```
POST /api/groups/{name}/health-check/{nodeName}
```

内部根据成员名称判断其来源（手动或订阅）。

### 4.4 顶层代理健康检查

#### POST /api/proxies/health-check/{name}

对全局代理列表中的单个代理执行 TCP 连通性检查。

响应：

```json
{
  "name": "MY_PROXY",
  "alive": true,
  "latencyMs": 45,
  "lastCheck": "2026-07-14T12:00:00Z"
}
```

## 5. 配置持久化规则

1. YAML 中只保存 `proxies`（手动成员列表）、`subscription`、`subscription-filter`、`active-member`；运行时 `Members` 不保存。
2. 编辑代理组时，手动成员顺序即 `proxies` 顺序。
3. `subscription-mode` 不再写入；`subscription-selected` 废弃，加载旧配置时迁移到 `active-member`。
4. 保存目标为 `base` 或 `env` 时，只写入选中的目标文件，然后触发 `mergeAndInitLocked` 重建运行时状态。
