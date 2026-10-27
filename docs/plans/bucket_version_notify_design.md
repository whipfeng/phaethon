# Bucket 粒度版本通知设计

**创建时间**: 2026-10-27  
**状态**: 设计中  
**优先级**: 高

## 背景

### 当前问题

1. **手动调用 BumpVersion**：每次数据库操作后需要在 admin API 中手动调用 `util.DefaultVersionNotifier.BumpVersion(topic)`，容易遗漏
2. **粒度不统一**：version topic 与 bucket 名称不一致，需要维护映射关系
3. **不一致风险**：数据库变更可能从多个入口触发（admin API、mesh 同步等），手动调用无法覆盖所有场景

### 设计目标

1. **自动化**：数据库变更自动触发版本通知，无需手动调用
2. **统一粒度**：以 bucket 为粒度，bucket 名称 = version topic
3. **简单一致**：新增 bucket 自动有对应 topic，无需修改代码
4. **前端灵活**：按需订阅，一个页面可以监听多个相关 bucket

## 设计方案

### 核心思路

监听所有 bucket 变更事件，自动调用 `BumpVersion(bucket_name)`。前端按需订阅关心的 bucket topic。

### Bucket 分类

#### 配置类 Bucket（需要版本通知）

| Bucket | Version Topic | 说明 |
|--------|--------------|------|
| `BucketAdminConfig` | `"admin-config"` | 管理配置（端口、认证） |
| `BucketTUNConfig` | `"tun-config"` | TUN 配置 |
| `BucketMeshConfig` | `"mesh-config"` | Mesh 网络配置 |
| `BucketInteractiveConfig` | `"interactive-config"` | 交互式配置 |
| `BucketPortRangeConfig` | `"port-range-config"` | UDP/TCP 端口范围 |
| `BucketProxies` | `"proxies"` | 代理配置 |
| `BucketProxyGroups` | `"proxy-groups"` | 代理组 |
| `BucketMappings` | `"mappings"` | 映射配置 |
| `BucketSubscriptions` | `"subscriptions"` | 订阅配置 |
| `BucketSubscriptionNodes` | `"subscription-nodes"` | 订阅节点池 |
| `BucketReverseConfs` | `"reverse-configs"` | 反向连接配置 |
| `BucketResolvers` | `"resolvers"` | DNS 解析器 |
| `BucketPackages` | `"packages"` | 套餐配置 |
| `BucketRules` | `"rules"` | 规则列表 |
| `BucketCertificates` | `"certificates"` | TLS 证书 |
| `BucketAPIKeys` | `"api-keys"` | API 密钥 |
| `BucketWebhooks` | `"webhooks"` | Webhook 配置 |
| `BucketLogs` | `"logs"` | 日志配置 |

#### 身份与状态 Bucket

| Bucket | Version Topic | 说明 |
|--------|--------------|------|
| `BucketInstanceIdentity` | `"instance-identity"` | 实例身份（node-id, reverse-id） |
| `BucketRuntimeState` | 跳过 | 运行时状态（高频变化） |

#### 运行时状态 Bucket（不需要版本通知）

| Bucket | 说明 |
|--------|------|
| `BucketFakeIP` | Fake-IP 映射（内存缓存，高频变化） |
| `BucketProxyHealth` | 代理健康检查（高频变化） |
| `BucketAuditLog` | 审计日志（只追加，不通知） |

### 实现方案

#### 后端：自动版本通知

```go
// db/version_notify.go
package db

import "phaethon/util"

// skipBuckets 不需要触发版本通知的 bucket（运行时状态）
var skipBuckets = map[string]bool{
    string(BucketFakeIP):          true,
    string(BucketProxyHealth):     true,
    string(BucketRuntimeState):    true,
    string(BucketAuditLog):        true,
}

// InitVersionNotifier 初始化版本通知订阅
// 在 main.go 中数据库初始化后调用
func InitVersionNotifier() {
    Subscribe(nil, func(event ChangeEvent) {
        if skipBuckets[event.Bucket] {
            return
        }
        // 直接用 bucket 名称作为 topic
        util.DefaultVersionNotifier.BumpVersion(event.Bucket)
    })
}
```

#### 前端：按需订阅

```javascript
// admin/static/app.js

function registerDefaultVersionHandlers() {
    // 配置类 bucket - 直接订阅
    onBusinessVersion('rules', () => scheduleTopicFetch('rules'), 'rules');
    onBusinessVersion('mappings', () => scheduleTopicFetch('mappings'), 'mappings');
    onBusinessVersion('config', () => scheduleTopicFetch('config'), 'config');
    onBusinessVersion('resolvers', () => scheduleTopicFetch('resolvers'), 'resolvers');
    onBusinessVersion('packages', () => scheduleTopicFetch('packages'), 'packages');
    
    // 代理页面 - 监听 proxies + proxy-groups
    onBusinessVersion('proxies', () => scheduleTopicFetch('proxies'), 'proxies');
    onBusinessVersion('proxy-groups', () => scheduleTopicFetch('proxies'), 'proxy-groups');
    
    // 订阅页面 - 监听 subscriptions + subscription-nodes
    onBusinessVersion('subscriptions', () => scheduleTopicFetch('subscriptions'), 'subscriptions');
    onBusinessVersion('subscription-nodes', () => scheduleTopicFetch('subscriptions'), 'subscription-nodes');
    
    // 反向连接 - 监听 reverse-configs
    onBusinessVersion('reverse-configs', () => scheduleTopicFetch('reverse'), 'reverse-configs');
    
    // 运行时状态 - 保持不变
    onBusinessVersion('tun', () => scheduleTopicFetch('tun'), 'tun');
    onBusinessVersion('mesh', () => fetchMeshStatus(), 'mesh');
    onBusinessVersion('p2p', () => {
        if (typeof window.fetchP2PStatus === 'function') {
            window.fetchP2PStatus();
        }
    }, 'p2p');
    onBusinessVersion('logs', () => {
        fetchConnections(true);
        fetchActiveConns(true);
        fetchStats();
    }, 'logs');
}
```

#### 初始化

```go
// main.go
func main() {
    // ... 初始化数据库 ...
    if err := db.Init(dbPath); err != nil {
        log.Fatal(err)
    }
    
    // 初始化版本通知（在数据库初始化后）
    db.InitVersionNotifier()
    
    // ... 其他初始化 ...
}
```

### 移除手动调用

从 admin.go 中移除所有手动调用的 `BumpVersion`：

```go
// 移除这些调用
// util.DefaultVersionNotifier.BumpVersion("rules")
// util.DefaultVersionNotifier.BumpVersion("proxies")
// util.DefaultVersionNotifier.BumpVersion("mappings")
// ...
```

## 数据迁移

### BucketConfig 拆分迁移

当前 `BucketConfig` 存储所有配置，需要拆分为多个 bucket：

```go
// 迁移脚本：tools/migrate-config-buckets.go
func migrateConfigBuckets() error {
    return db.Update(func(tx *bolt.Tx) error {
        oldBucket := tx.Bucket([]byte("config"))
        if oldBucket == nil {
            return nil
        }
        
        // 读取所有配置
        var adminConfig, tunConfig, meshConfig, interactiveConfig, portRangeConfig []byte
        
        oldBucket.ForEach(func(k, v []byte) error {
            switch string(k) {
            case "admin":
                adminConfig = v
            case "tun":
                tunConfig = v
            case "mesh":
                meshConfig = v
            case "interactive":
                interactiveConfig = v
            case "udp-port-range", "tcp-port-range":
                portRangeConfig = v
            }
            return nil
        })
        
        // 写入新 bucket
        if adminConfig != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("admin-config"))
            b.Put([]byte("data"), adminConfig)
        }
        if tunConfig != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("tun-config"))
            b.Put([]byte("data"), tunConfig)
        }
        if meshConfig != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("mesh-config"))
            b.Put([]byte("data"), meshConfig)
        }
        if interactiveConfig != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("interactive-config"))
            b.Put([]byte("data"), interactiveConfig)
        }
        if portRangeConfig != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("port-range-config"))
            b.Put([]byte("data"), portRangeConfig)
        }
        
        // 删除旧 bucket
        tx.DeleteBucket([]byte("config"))
        
        return nil
    })
}
```

### BucketState 拆分迁移

当前 `BucketState` 存储身份和运行时状态，需要拆分：

```go
// 迁移脚本：tools/migrate-state-buckets.go
func migrateStateBuckets() error {
    return db.Update(func(tx *bolt.Tx) error {
        oldBucket := tx.Bucket([]byte("state"))
        if oldBucket == nil {
            return nil
        }
        
        // 读取所有状态
        var identityData, runtimeData []byte
        
        oldBucket.ForEach(func(k, v []byte) error {
            key := string(k)
            if key == "node-id" || key == "reverse-id" {
                // 身份数据
                if identityData == nil {
                    identityData = make([]byte, 0)
                }
                identityData = append(identityData, v...)
            } else {
                // 运行时数据
                if runtimeData == nil {
                    runtimeData = make([]byte, 0)
                }
                runtimeData = append(runtimeData, v...)
            }
            return nil
        })
        
        // 写入新 bucket
        if identityData != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("instance-identity"))
            b.Put([]byte("data"), identityData)
        }
        if runtimeData != nil {
            b, _ := tx.CreateBucketIfNotExists([]byte("runtime-state"))
            b.Put([]byte("data"), runtimeData)
        }
        
        // 删除旧 bucket
        tx.DeleteBucket([]byte("state"))
        
        return nil
    })
}
```

### 迁移步骤

1. **停止服务**
2. **备份数据库**：`cp phaethon.db phaethon.db.backup-before-migration`
3. **运行迁移工具**：`./tools/migrate-config-buckets && ./tools/migrate-state-buckets`
4. **验证迁移**：检查新 bucket 是否存在，旧 bucket 是否已删除
5. **启动服务**
6. **验证功能**：检查配置是否正常加载

## 影响范围

### 后端改动

1. **新增文件**：`db/version_notify.go`
2. **修改 main.go**：添加 `db.InitVersionNotifier()` 调用
3. **修改 admin.go**：移除所有手动 `BumpVersion` 调用

### 前端改动

1. **修改 app.js**：更新 `registerDefaultVersionHandlers()` 中的 topic 订阅
2. **修改 fetch 函数**：确保 `fetchForTopic` 能处理新的 topic 名称

### 兼容性

- **向后兼容**：旧的 topic 名称（如 `"proxies"`）仍然有效，只是需要额外订阅相关 bucket
- **渐进式迁移**：可以先实现后端自动通知，前端逐步调整订阅

## 测试计划

### 单元测试

1. **测试 InitVersionNotifier**：验证 bucket 变更触发 BumpVersion
2. **测试 skipBuckets**：验证运行时状态 bucket 不触发通知

### 集成测试

1. **规则变更**：添加/删除/修改规则 → 前端自动刷新
2. **代理变更**：添加/删除/修改代理 → 前端自动刷新
3. **映射变更**：添加/删除/修改映射 → 前端自动刷新
4. **订阅变更**：添加/删除/修改订阅 → 前端自动刷新

### 手动测试

1. 在控制台添加一条规则 → 页面自动刷新，新规则出现
2. 在控制台删除一条规则 → 页面自动刷新，规则消失
3. 在控制台修改代理配置 → 代理页面自动刷新
4. 检查浏览器控制台 → 无 JavaScript 错误

## 风险与缓解

### 风险 1：性能影响

**问题**：notifyChanges 是同步调用，BumpVersion 需要快速返回

**缓解**：
- BumpVersion 本身很快（递增计数器 + 广播）
- 如果性能有问题，可以改为异步调用（goroutine）

### 风险 2：重复通知

**问题**：已有 `Subscribe(nil, onConfigChange)` 监听所有 bucket，再加一个订阅者会有两个

**缓解**：
- 两个订阅者各自独立，互不影响
- onConfigChange 更新 RuleMatcher，版本通知触发前端刷新，职责不同

### 风险 3：前端 topic 名称变更

**问题**：前端需要从旧 topic 名称迁移到新 topic 名称

**缓解**：
- 渐进式迁移：先实现后端，前端逐步调整
- 兼容期：可以同时订阅新旧 topic（短期）

## 时间估算

1. **后端实现**：30 分钟
2. **前端调整**：30 分钟
3. **测试验证**：30 分钟
4. **总计**：1.5 小时

## 后续优化

1. **监控**：添加日志记录版本通知触发次数，监控性能
2. **优化**：如果性能有问题，可以批量合并短时间内的多次通知
3. **文档**：更新开发者文档，说明 bucket 与 version topic 的对应关系

## 参考资料

- [RuleMatcher 设计文档](./rule_matcher_design.md)
- [VersionNotifier 实现](../util/version.go)
- [数据库通知机制](../db/notify.go)
