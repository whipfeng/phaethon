# 嵌入式数据库设计（bbolt）

## 背景

当前 Phaethon 使用文件系统存储数据：
- 配置：YAML 文件
- 包元数据：文件系统扫描
- Fake-IP 映射：内存（重启丢失）
- 连接日志：内存/文件

**问题：**
- 包元数据查询慢（需要扫描目录）
- Fake-IP 映射无法持久化
- 配置修改需要重启
- 缺乏统一的数据存储

## 目标

引入嵌入式数据库（bbolt），实现：
1. 统一数据存储（配置 + 状态 + 历史）
2. 动态配置修改（无需重启）
3. 数据持久化（跨重启）
4. 快速查询（替代文件扫描）

## 技术选型

**选择：bbolt**

**理由：**
- 纯 Go（无 CGO 依赖）
- 支持 mmap（性能好）
- 简单 KV 存储（满足需求）
- etcd 底层使用（稳定可靠）
- 体积小（~1-2 MB）

**替代方案：**
- Badger：功能更多，但体积更大
- SQLite：需要 CGO
- LMDB：C 库，跨平台复杂

## 架构：数据库为唯一事实源

### 核心原则

**数据库是唯一事实源（per-key 细粒度存储）。** 不存在 `*config.RuleConfiguration` 这样的中间结构。各组件按需直接查询数据库，缓存按 bucket 单独设计（如需要）。

### 数据流向

```
启动 / OnReload（读取路径）:
  各组件直接调用 db.GetXxx() 查询所需配置
    → Admin 服务器: db.GetAdminConfig()
    → Mesh 子系统: db.GetMeshConfig()
    → TUN 引擎: db.GetTUNConfig()
    → 规则路由: db.ListRules()
    → 代理管理: db.ListProxies()
    → 映射管理: db.ListMappings()
    → ...

轻量变更（控制台编辑，热更新路径）:
  API 请求 → 校验
    → db.PutXxx(item)                             # 细粒度落库（持久化）
    → OnReload（通知相关组件重新加载）              # 组件从 DB 重新查询
    → BumpVersion("config")                       # 通知前端刷新
```

### 为什么废除中间结构

- **消除数据不一致**：没有内存快照与数据库的同步问题
- **简化代码**：不需要维护 mergeAndInitLocked() 等复杂逻辑
- **精确控制**：每个组件只查询自己需要的数据，避免加载无关配置
- **按需缓存**：如某组件需要频繁读取，可单独为该 bucket 设计缓存（如 ProxyCache、MappingCache）

### 缓存设计原则（按需）

如果某 bucket 的数据被频繁读取，可为该 bucket 单独设计缓存：

```go
// 示例：ProxyCache 按需缓存 proxies bucket
type ProxyCache struct {
    mu      sync.RWMutex
    proxies map[string]*config.Proxy
    dirty   bool
}

func (c *ProxyCache) Get(name string) (*config.Proxy, error) {
    c.mu.RLock()
    if !c.dirty {
        if p, ok := c.proxies[name]; ok {
            c.mu.RUnlock()
            return p, nil
        }
    }
    c.mu.RUnlock()
    
    // Cache miss or dirty, query DB
    p, err := db.GetProxy(name)
    if err != nil {
        return nil, err
    }
    
    c.mu.Lock()
    c.proxies[name] = p
    c.dirty = false
    c.mu.Unlock()
    
    return p, nil
}

func (c *ProxyCache) Invalidate() {
    c.mu.Lock()
    c.dirty = true
    c.mu.Unlock()
}
```

**关键点**：
- 缓存按 bucket 独立设计，不是全量缓存
- 提供 Invalidate() 方法，API 修改后调用
- 缓存是可选优化，不是必须

### 禁止事项

- 禁止任何代码绕过 admin API 直接修改运行时配置
- 禁止后台任务将运行时 conf 静默回写 DB（所有配置变更必须通过细粒度 API）
- YAML 仅作为导出格式（`/api/config/raw` GET=导出），不再是运行时配置来源，不支持批量导入
- 禁止使用 `*config.RuleConfiguration` 作为中间结构传递配置（应直接查询数据库）

## 根基设置（Foundational Settings）

### 定义

根基设置是节点身份与访问入口的核心配置，**init 交互过程中确定，reset 不覆盖，但提供单独 API 修改**：

| 根基设置 | 存储位置 | 说明 |
|---------|---------|------|
| Mesh node-id | config bucket, key=mesh | 节点在 mesh 网络中的唯一标识，决定 VIP 与 subnet |
| Mesh subnet | config bucket, key=mesh | 节点管理的 VIP 地址范围 |
| Admin port | config bucket, key=admin | Admin API 监听端口 |
| Admin auth | config bucket, key=admin | Admin API 认证配置（username/password/token） |

### 根基设置修改 API

根基设置通过独立 API 修改，不支持批量重置：

```
PUT /api/config/mesh
  → 更新 mesh 配置（node-id、subnet）
  → db.PutMeshConfig(mesh)
  → OnReload（重启 mesh 子系统：重新通告、重建路由表）
  → 警告：修改 node-id 会改变节点身份，其他节点需要重新发现

PUT /api/config/admin
  → 更新 admin 配置（port、auth）
  → db.PutAdminConfig(admin)
  → OnReload（重启 admin 服务器：重新绑定端口、更新认证中间件）
  → 警告：修改 port 会改变访问地址
```

**热生效机制**：根基设置修改触发 OnReload（全量重建），因为：
- Mesh 身份变化需要重启 P2P、重建路由表、重新通告
- Admin 端口变化需要重新绑定监听器

OnReload 是现有机制，代价可接受（根基设置修改频率极低）。

### 禁止事项（补充）

- 禁止绕过 API 直接修改根基设置（必须通过 PUT /api/config/mesh 或 PUT /api/config/admin）
- 禁止批量导入/重置配置（所有配置项通过细粒度 API 逐个管理）

## 数据模型

**存储编码**：统一 JSON（config 包结构体自带 json tag）。**config 包是唯一类型来源**，db 包不定义重复类型。

### Bucket 一览

| Bucket | Key | Value 类型 | 说明 |
|--------|-----|-----------|------|
| `config` | `admin` | config.AdminConfig | Admin 面板（addr 格式 `0.0.0.0:39999`） |
| `config` | `tun` | config.TUNConfig | TUN 设置 |
| `config` | `mesh` | config.MeshConfig | Mesh 设置（强制启用，node-id 必填） |
| `config` | `interactive` | bool | 交互式引导开关 |
| `config` | `udp-port-range` | string | `"min-max"` |
| `config` | `tcp-port-range` | string | `"min-max"` |
| `proxies` | `{name}` | config.Proxy | 手动代理 |
| `proxy-groups` | `{name}` | config.ProxyGroup | 手工成员以 ManualProxies 为准 |
| `subscriptions` | `{name}` | config.Subscription | URL + interval |
| `rules` | `%06d` 索引 | string | 原始规则串，如 `"DOMAIN-SUFFIX,google.com,GG_PROXY"` |
| `mappings` | `{name}` | config.Mapping | 入站映射 |
| `resolvers` | `{name}` | config.Resolver | DNS 解析规则 |
| `reverse-configs` | `{name}` | config.ReverseConfig | 反向客户端配置（Name 唯一，由 uniqueReverseName 保证） |
| `fakeip` | `domain:{domain}` / `ip:{ip}` | FakeIPEntry / `{domain}` | 双向索引 |
| `packages` | `{platform}:{arch}:{version}` | PackageMeta | 包元数据 |

### 运行时字段不入库（持久化前清零）

| 类型 | 清零字段 | 原因 |
|------|----------|------|
| config.ProxyGroup | `Proxies`、`Members`、`ManualMembers`、`SubMembers`、`SubCandidateCount`、`SubscriptionSelected`、`SubscriptionMode` | 运行时派生 / 已废弃（Init 会从 `proxies` 键迁移到 ManualProxies） |
| config.ReverseConfig | `LastError`、`AssignedPort` | 运行时信息 |
| config.Proxy | 无需处理 | `Next`/RateLimiter/`SourceGroup` 本身就是 `json:"-"` |
| config.Subscription | 无需处理 | `SubProxies`/`SubMu` 是 `json:"-"` |

代理组的手工成员以 `ManualProxies` 为准：YAML 旧格式（`proxies:` 键）在 `config.ProxyGroup.Init()` 中已迁移，DB 加载路径 ManualProxies 直接可用，无需二次迁移。

### 变更事件语义

- **per-key 写入/删除**：`Put`/`Delete` 触发对应 bucket 的 ChangeEvent（细粒度）
- **规则列表更新**（`ReplaceAllRules`，规则增删改查走此入口）：单个写事务原子替换规则 bucket（先清空再写入），提交后触发一次合成事件 `{bucket: "rules", key: "__rebuilt__", op: "put"}`
- 配置热更新**不依赖** watcher（原地修改共享 conf 即时生效）；变更事件面向后续阶段的订阅者（如 Fake-IP 持久化、审计）与未来的外部集成

**不持久化的数据：**
- ✗ Peer 历史：内存维护当前连接的 peer，通过 mesh gossip 重新发现
- ✗ 连接日志：使用现有文件系统日志（轮转），避免数据库过大

## 初始化流程

### 首次启动

```bash
$ phaethon
INFO: 数据库不存在，进入初始化模式

? Admin API 端口 (默认 39999): 39999
? Mesh node-id (留空自动生成): 
INFO: 生成 Mesh node-id: 15538109650403028488
INFO: 数据库创建完成：phaethon.db
INFO: 启动服务...
```

**注意**：如果工作目录存在旧版 `config.yaml`，程序会自动迁移到数据库并跳过交互初始化。迁移完成后，`config.yaml` 不再使用，所有配置通过控制台 UI 或 Admin API 管理。

### 后续启动

```bash
$ phaethon
INFO: 加载配置 from phaethon.db
INFO: Mesh node-id: 15538109650403028488
INFO: Admin API 监听 :39999
INFO: 启动服务...
```

## 实现计划

### Phase 1：基础设施

1. 引入 bbolt 依赖
2. 实现数据库初始化工具
3. 实现首次启动交互流程
4. 实现配置加载/保存

### Phase 2：数据迁移

1. 包元数据迁移（文件系统 → 数据库）✅
2. Fake-IP 映射持久化（详见 2.1 节）
3. 配置数据迁移（YAML → 数据库）✅

#### 2.1 Fake-IP 映射持久化设计

**目标**：重启后保留域名到 Fake-IP 的映射，便于会话跟踪和问题诊断。

**数据模型**（已实现）：
```go
type FakeIPEntry struct {
    IP        string    `json:"ip"`
    Domain    string    `json:"domain"`
    CreatedAt time.Time `json:"created_at"`
    LastUsed  time.Time `json:"last_used"`
}
```

**存储结构**：
- Bucket: `fakeip`
- 双向索引：
  - `domain:{domain}` → `FakeIPEntry`
  - `ip:{ip}` → `{"domain": domain}`

**集成点**：

| 操作 | 触发时机 | db 操作 |
|------|----------|---------|
| 分配 | `FakeIPPool.Lookup()` 首次分配新 IP | `PutFakeIP(domain, ip)` |
| 查询 | `FakeIPPool.Lookup()` 域名已存在 | 无（内存命中） |
| 加载 | `FakeIPPool` 初始化 | 遍历 `fakeip` bucket 加载到内存 |
| 删除 | `FakeIPPool.Release()` | `DeleteFakeIP(domain, ip)` |

**性能考虑**：
- 写入频率：每次新域名查询写一次 db（非高频）
- 启动加载：一次性加载所有映射到内存
- `LastUsed` 更新：暂不持久化（避免每次使用都写 db）

**错误处理**：
- db 写入失败：记录警告，继续运行（内存映射仍可用）
- db 加载失败：记录警告，从空池开始（不影响功能）

**过期清理**（后续优化）：
- 可添加 Admin API 手动清理
- 或按 `CreatedAt` 自动清理超过 N 天的记录

### Phase 3：功能增强

1. Admin API 动态配置修改
2. 配置导入/导出（YAML）
3. Peer 历史记录
4. 连接日志持久化

### Phase 4：优化

1. 查询优化
2. 缓存策略
3. 性能监控

## 迁移策略

### 从 YAML 迁移到数据库

**方案 A：自动迁移（推荐）**
```
首次启动新版：
  → 检测 config.yaml 存在
  → 检测 phaethon.db 不存在
  → 自动导入 config.yaml → phaethon.db
  → 重命名 config.yaml → config.yaml.bak
```

**方案 B：手动迁移**
```
phaethon --import ./config.yaml
  → 创建数据库
  → 导入配置
```

### 从文件系统迁移包元数据

```
启动时：
  → 扫描 data/packages/ 目录
  → 提取包元数据
  → 写入数据库
  → 标记已迁移
```

## Admin API 扩展

### 配置管理（CRUD 完整覆盖）

```
# 代理配置
GET    /api/config/proxies           # 列出所有代理
GET    /api/config/proxies/{name}    # 查看单个代理
POST   /api/config/proxies           # 添加代理
PUT    /api/config/proxies/{name}    # 更新代理
DELETE /api/config/proxies/{name}    # 删除代理

# 规则配置
GET    /api/config/rules             # 列出所有规则（按索引排序）
GET    /api/config/rules/{index}     # 查看单个规则
POST   /api/config/rules             # 添加规则
PUT    /api/config/rules/{index}     # 更新规则（支持调整顺序）
DELETE /api/config/rules/{index}     # 删除规则

# Mesh 配置
GET    /api/config/mesh              # 查看 Mesh 配置
PUT    /api/config/mesh              # 更新 Mesh 配置

# Admin 配置
GET    /api/config/admin             # 查看 Admin 配置
PUT    /api/config/admin             # 更新 Admin 配置
```

### 状态数据查看

```
# Fake-IP 映射
GET    /api/fakeip                   # 列出所有 Fake-IP 映射
GET    /api/fakeip/{domain}          # 查看指定域名映射
DELETE /api/fakeip/{domain}          # 删除指定映射（强制释放）
```

### 包管理（已有 + 数据库化）

```
GET    /api/packages                 # 列出所有包（从数据库查询）
GET    /api/packages/{platform}/{arch}/{version}  # 查看包详情
DELETE /api/packages/{platform}/{arch}/{version}  # 删除包
```

### 数据备份

```
POST /api/export    # 导出配置到 YAML
POST /api/import    # 从 YAML 导入配置
```

### 控制台页面映射

| 页面 | 数据来源 | 操作 |
|------|---------|------|
| 代理管理页 | BucketProxies | 查看/添加/编辑/删除 |
| 规则管理页 | BucketRules | 查看/添加/编辑/删除/排序 |
| Mesh 页面 | BucketConfig(mesh) | 查看/编辑 |
| 系统设置页 | BucketConfig(admin) | 查看/编辑 |
| Fake-IP 页面 | BucketFakeIP | 查看/搜索/删除 |
| 包管理页 | BucketPackages | 查看/上传/删除/分发 |
| 导出配置 | 整库 | YAML 导出（仅导出，不支持批量导入） |

## 优势

1. **统一管理**：所有数据在数据库
2. **动态配置**：修改立即生效，无需重启
3. **持久化**：Fake-IP、Peer 历史跨重启
4. **快速查询**：替代文件扫描
5. **未来扩展**：新功能直接加表

## 风险

1. **数据库损坏**：需要备份机制
2. **迁移失败**：需要回滚方案
3. **性能问题**：需要监控
4. **兼容性**：旧版本无法读取新数据库

## 回滚方案

```
如果新版出问题：
  → 导出配置：POST /api/export > config.yaml
  → 降级到旧版
  → 使用 config.yaml 启动
```

## 时间估算

- Phase 1（基础设施）：1-2 天
- Phase 2（数据迁移）：1 天
- Phase 3（功能增强）：2-3 天
- Phase 4（优化）：1 天

**总计：5-7 天**

## 实现状态（2026-10-14 更新）

### 已完成

1. **数据库基础设施** ✅
   - bbolt 集成（mmap 内存映射，读操作为内存速度）
   - 所有 bucket 实现（config, proxies, proxy-groups, subscriptions, rules, mappings, resolvers, reverse-configs, fakeip, packages）
   - 事务支持（View/Update）
   - 变更事件机制

2. **Admin API 直接操作数据库** ✅
   - 规则管理 API（GET/POST/DELETE/PATCH /api/rules）直接调用 `db.ListRules()`、`db.PutRule()`、`db.DeleteRuleByIndex()` 等
   - 绕过 `LoadConf()` 避免配置验证死锁
   - 代理、映射、订阅等 CRUD API 直接操作对应 bucket
   - 配置导出 API（/api/config/raw）仍使用 `LoadConf()` 生成完整 YAML

3. **数据迁移** ✅
   - YAML → 数据库自动迁移（首次启动）
   - 包元数据迁移（文件系统 → 数据库）
   - Fake-IP 持久化

### 进行中

**消除 RuleConfiguration 中间层** 🚧

当前状态：
- `config.RuleConfiguration` 结构体仍然存在，包含 Proxies、ProxyGroups、Rules、Mappings 等字段
- `db.LoadConf()` 函数仍然存在，从数据库加载全量配置到 RuleConfiguration 结构体
- 92 处代码引用 `ruleConf` 变量或字段（分布在 10 个文件中）
- 主要使用场景：
  - `main.go`: 启动时加载配置，传递给各组件
  - `admin/admin.go`: reloadRuntimeLocked() 重新加载配置
  - 各组件：通过 `conf.Proxies`、`conf.Rules` 等方式访问配置

目标架构：
- 各组件直接查询数据库（`db.ListProxies()`、`db.ListRules()` 等）
- 按需缓存（如 ProxyCache、RuleCache），缓存按 bucket 独立设计
- 消除 `*config.RuleConfiguration` 作为中间传递结构

迁移计划：
1. **Phase A: 组件级直接查询**（优先级：高）
   - 识别各组件的配置访问模式
   - 为高频读取的 bucket 实现按需缓存（如 ProxyCache）
   - 逐步替换 `conf.Proxies` → `db.ListProxies()` 或 `proxyCache.Get(name)`
   - 逐步替换 `conf.Rules` → `db.ListRules()` 或 `ruleCache.List()`

2. **Phase B: 消除 LoadConf()**（优先级：中）
   - 当所有组件都直接查询数据库后，`LoadConf()` 仅用于配置导出
   - 重命名 `LoadConf()` → `ExportConfig()`，明确其用途
   - 删除 `activeConf`、`res.conf` 等全局/运行时配置传递

3. **Phase C: 清理 RuleConfiguration 结构体**（优先级：低）
   - 保留 `config.RuleConfiguration` 仅作为配置导出/导入的数据结构
   - 或者完全删除，导出时直接构造 JSON/YAML

### 性能考虑

**BoltDB mmap 性能**：
- 读操作通过 mmap 直接访问内存映射文件，速度接近内存读取
- 无需额外缓存层（RuleConfiguration）
- 单次读操作延迟：< 1μs（内存速度）
- 全量加载 1000 条规则：< 1ms

**缓存策略**：
- 仅在以下情况引入缓存：
  - 某 bucket 的数据被极高频读取（如每秒数千次）
  - 需要复杂的内存索引（如按名称快速查找代理）
- 缓存必须提供 Invalidate() 方法，API 修改后调用
- 缓存是可选优化，不是必须

### 禁止事项（强化）

- ❌ 禁止新增使用 `*config.RuleConfiguration` 作为中间传递结构的代码
- ❌ 禁止在 API handler 中调用 `LoadConf()` 读取配置（应直接查询数据库）
- ❌ 禁止维护全局的 `activeConf`、`res.conf` 或 `s.runtime`（各组件应直接查询数据库）
- ❌ 禁止在启动时加载完整配置传递给各组件（各组件应查询自己需要的配置）
- ❌ 禁止为了导出配置而加载完整RuleConfiguration结构（应直接查询各bucket构造导出）
- ✅ 新增功能应直接查询数据库（`db.GetXxx()`、`db.ListXxx()`）
- ✅ 高频读取场景应设计按需缓存（按 bucket 独立）
- ✅ 运行时状态（如健康信息）应与配置分离，单独存储在内存map中

### 架构重构记录（2026-10-23）

**已完成的优化：**

1. **Admin API 直接查询数据库** ✅
   - `findProxyReferences()` - 使用 `db.ListProxies()`, `db.ListRules()`, `db.ListMappings()`, `db.ListReverseConfigs()`
   - `findGroupReferences()` - 使用 `db.ListProxyGroups()`, `db.ListRules()`, `db.ListProxies()`, `db.ListMappings()`, `db.ListReverseConfigs()`
   - Rules API 端点 - 使用 `db.ListRules()`, `db.PutRule()`, `db.DeleteRuleByIndex()`
   - 消除了配置验证死锁问题

2. **消除配置导出API的全量加载** ✅
   - `apiConfig()` 已删除（冗余API）
   - `apiConfigRaw()` 直接查询各bucket构造RuleConfiguration
   - 前端改用细粒度API（`/api/config/admin`, `/api/config/mesh`等）

3. **UI清理：config页面改为只读导出** ✅
   - 移除编辑功能（PUT /api/config/raw已不支持）
   - 移除重置功能（POST /api/config/reset已不存在）
   - 改为只读显示+复制+下载YAML

**待实施的重构：**

4. **运行时状态全部落库** 🚧 待实施
   - **核心原则**：数据库是唯一事实源，所有状态（配置+运行时）都存储在数据库
   - **新增bucket**：
     - `proxy_health` - 代理健康状态（alive, latency, lastCheck, failCount）
     - `subscription_nodes` - 订阅节点池（从订阅URL拉取的节点列表）
   - **更新时机**：
     - 健康检查完成后，异步批量写入 `proxy_health`
     - 订阅拉取完成后，写入 `subscription_nodes`
   - **优势**：
     - 重启后自动恢复所有状态
     - 无内存缓存，无一致性问题
     - 代码简化，不需要维护 `s.runtime`
   
   ```go
   // 健康状态存储
   // Bucket: proxy_health
   // Key: {groupName}:{proxyName}
   // Value: JSON {alive, latency, lastCheck, failCount}
   db.PutProxyHealth(groupName, proxyName, &ProxyHealth{...})
   health, _ := db.GetProxyHealth(groupName, proxyName)
   healths, _ := db.ListProxyHealthByGroup(groupName)
   
   // 订阅节点池存储
   // Bucket: subscription_nodes
   // Key: subscriptionName
   // Value: JSON {nodes: [...], lastUpdate: ...}
   db.PutSubscriptionNodes(subName, nodes)
   nodes, _ := db.GetSubscriptionNodes(subName)
   ```

5. **消除启动时全量加载** 🚧 待实施
   - 当前：`main.go` 启动时调用 `db.LoadConf()` 加载完整配置，传递给各组件
   - 目标：各组件自己查询需要的配置
   ```go
   // 重构前
   conf, _ := db.LoadConf()
   startEngine(conf, ...)
   server.StartTrojan(conf, ...)
   
   // 重构后
   meshCfg := db.GetMeshConfig()
   tunCfg := db.GetTUNConfig()
   proxies, _ := db.ListProxies()
   mappings, _ := db.ListMappings()
   startEngine(meshCfg, tunCfg, ...)
   server.StartTrojan(meshCfg, mapping, ...)
   ```

6. **消除运行时缓存 s.runtime** 🚧 待实施
   - 当前：`AdminServer.runtime` 保存完整配置缓存，reload时保留健康状态
   - 目标：完全消除缓存，每次查询DB
   ```go
   // 重构前
   type AdminServer struct {
       runtime *config.RuleConfiguration  // 完整配置缓存
   }
   
   // 重构后
   type AdminServer struct {
       // 无runtime字段，按需查询DB
   }
   // 需要ProxyGroups时直接查询DB
   groups, _ := db.ListProxyGroups()
   // 需要健康状态时查询proxy_health bucket
   health, _ := db.GetProxyHealth(groupName, proxyName)
   ```

## 动态配置融合机制

### 背景

反向连接（reverse connection）注册时，会动态生成以下配置：
- **动态 Proxy**：`dyn-proxy-{hexID}`，用于反向代理
- **动态 Mapping**：`dyn-{hexID}`，映射到反向端地址
- **动态 Rule**：`MATCH,dyn-proxy-{hexID}#dyn-{hexID}`，路由规则
- **动态 Matcher**：编译后的规则匹配器

这些动态配置的特点：
- **参与匹配**：需要和静态配置一起参与规则匹配
- **不持久化**：不应该写入数据库（反向端断开后应自动清理）
- **运行时注入**：由 `ControlManager` 在反向连接注册时创建

### 问题

重构后使用 `db.RuleMatcher` 从数据库构建匹配规则，但：
1. 动态配置不在数据库中
2. `RuleMatcher` 重建时会丢失动态配置
3. 需要一种机制让动态配置参与匹配

### 方案：统一对象 Map + Watch 同步

**核心思路**：所有配置（静态+动态）统一存储在对象 Map 中，通过 Watch 实时同步 DB 变更，无需 rebuild，无需关闭连接。

#### 数据结构

```go
type RuleMatcher struct {
    // 统一对象 Map（包含静态+动态）
    proxies       map[string]*config.Proxy
    groups        map[string]*config.ProxyGroup
    mappings      map[string]*config.Mapping
    subscriptions map[string]*config.Subscription  // 包含节点池
    rules         []*config.Rule                    // 结构化对象
    
    mu sync.RWMutex  // 保护并发访问
}

// config.Rule 结构化定义
type Rule struct {
    Type        string      // DOMAIN, DOMAIN-SUFFIX, IP-CIDR, MATCH 等
    Pattern     string      // 匹配模式
    ProxyName   string      // 目标代理名称
    MappingName string      // 可选映射名称
    TimeRange   *TimeRange  // 可选时间范围
}
```

#### 设计原则

1. **统一存储**：静态和动态对象存在同一个 Map，不区分来源
2. **名称引用**：所有关联使用名称（如 `ProxyGroup.Subscription`），不持有对象引用
3. **Watch 同步**：DB 变更时 Watch 直接更新 Map，不触发 rebuild
4. **无重建**：没有"重建整个 RuleMatcher"的概念，只有增量更新

#### Watch 处理逻辑

```go
func onDBChange(event ChangeEvent) {
    matcher.mu.Lock()
    defer matcher.mu.Unlock()
    
    switch event.Bucket {
    case BucketProxies:
        if event.Op == "delete" {
            delete(matcher.proxies, event.Key)
        } else {
            proxy := loadProxyFromDB(event.Key)
            matcher.proxies[event.Key] = proxy
        }
        
    case BucketProxyGroups:
        // 类似处理
        
    case BucketSubscriptions:
        // 更新订阅（包含节点池）
        
    case BucketSubscriptionNodes:
        // 更新节点池
        
    case BucketMappings:
        // 类似处理
        
    case BucketRules:
        // 规则变更：重新解析规则字符串为 Rule 对象
        matcher.rules = parseRulesFromDB()
    }
}
```

#### 动态对象生命周期

```go
// 反向连接注册时
func (m *ControlManager) insertRoutingRule(...) {
    matcher.mu.Lock()
    defer matcher.mu.Unlock()
    
    // 直接加入统一 Map
    matcher.proxies[proxy.Name] = proxy
    matcher.mappings[mapping.Name] = mapping
    matcher.rules = append(matcher.rules, rule)
}

// 反向连接断开时
func (m *ControlManager) HandleClose(...) {
    matcher.mu.Lock()
    defer matcher.mu.Unlock()
    
    // 直接从统一 Map 移除
    delete(matcher.proxies, proxyName)
    delete(matcher.mappings, mappingName)
    // 从 rules 中移除对应规则
}
```

#### 优点

- ✅ **无中断**：DB 变更不关闭控制连接，反向连接不中断
- ✅ **实时同步**：Watch 立即更新，无延迟
- ✅ **简洁统一**：静态/动态对象同等对待，代码简单
- ✅ **性能优化**：规则预解析为对象，匹配效率高
- ✅ **松耦合**：名称引用，对象独立更新互不影响

#### 缺点

- ⚠️ **并发控制**：需要仔细处理 Map 的并发访问（已用 mutex 保护）
- ⚠️ **内存占用**：所有对象常驻内存（但配置量小，可接受）

### 影响分析

#### 代理组管理逻辑

**当前实现**：
- `ProxyGroup` 通过 `ResolveSubscription` 回调查找订阅
- 回调从 `RuleMatcher.subscriptionNames` 缓存查找
- 订阅变更触发 rebuild，更新缓存

**新方案影响**：
- ✅ **无影响**：`ResolveSubscription` 回调改为从 `matcher.subscriptions` Map 查找
- ✅ **更简单**：订阅变更直接更新 Map，ProxyGroup 自动拿到最新数据
- ✅ **无需 rebuild**：ProxyGroup 的成员列表可以在 `Next()` 时实时查询订阅节点池

**实现方式**：
```go
// ProxyGroup 只保存名称引用
type ProxyGroup struct {
    Subscription string  // 订阅名称
    // ...
}

// Next() 时实时查询
func (g *ProxyGroup) Next() *config.Proxy {
    // 从 matcher.subscriptions 获取最新订阅和节点池
    sub := matcher.subscriptions[g.Subscription]
    nodes := sub.SubProxies
    // 应用 filter，选择节点
}
```

#### 健康检查逻辑

**当前实现**：
- 健康检查结果存在 `ProxyGroup.healthMap`（内存）
- 不持久化到 DB
- 重启后重新检查

**新方案影响**：
- ✅ **无影响**：健康检查已经是内存化，符合设计
- ✅ **更简单**：无需考虑 rebuild 时如何保留健康状态

**实现方式**：
```go
type ProxyGroup struct {
    healthMap map[string]*healthStatus  // 内存，不持久化
    healthMu  sync.RWMutex
}

// 健康检查更新
func (g *ProxyGroup) UpdateHealth(proxyName string, status *healthStatus) {
    g.healthMu.Lock()
    defer g.healthMu.Unlock()
    g.healthMap[proxyName] = status
}
```

### 实施计划

1. **重构 RuleMatcher 数据结构**
   - 改为统一对象 Map
   - 规则改为结构化对象 `[]*config.Rule`
   
2. **实现 Watch 同步逻辑**
   - 处理 Insert/Update/Delete 事件
   - 直接更新 Map，不触发 rebuild
   
3. **移除 rebuild 机制**
   - 删除 `doRebuildMatcher()`
   - 删除 `RebuildHook` 回调
   - 删除 `shouldRebuildBucket()`
   
4. **修改动态对象注入逻辑**
   - `ControlManager` 直接操作统一 Map
   - 加锁保护并发访问
   
5. **测试验证**
   - DB 变更不中断反向连接
   - 动态配置正确参与匹配
   - 代理组和健康检查正常工作

### 实现要点

**1. RuleMatcher 支持动态配置注入**

```go
type RuleMatcher struct {
    // 静态配置（从 DB 加载）
    proxies       []*config.Proxy
    rules         []string
    // ...
    
    // 动态配置（运行时注入）
    dynamicProxies  []*config.Proxy
    dynamicRules    []string
    dynamicMatchers []config.Matcher
    
    // 统一查找表（静态 + 动态）
    proxyNames map[string]*config.Proxy
    matchers   []config.Matcher
}

// AddDynamicConfig 注入动态配置（由 ControlManager 调用）
func (m *RuleMatcher) AddDynamicConfig(proxy *config.Proxy, rule string, matcher config.Matcher) {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    m.dynamicProxies = append(m.dynamicProxies, proxy)
    m.dynamicRules = append(m.dynamicRules, rule)
    m.dynamicMatchers = append(m.dynamicMatchers, matcher)
    
    // 重建查找表
    m.rebuildLookupTables()
}

// ClearDynamicConfig 清空动态配置（重建时调用）
func (m *RuleMatcher) ClearDynamicConfig() {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    m.dynamicProxies = nil
    m.dynamicRules = nil
    m.dynamicMatchers = nil
    
    m.rebuildLookupTables()
}
```

**2. DB 变更时关闭控制连接**

```go
// db/matcher.go
func rebuildMatcher(event ChangeEvent) {
    bucket := string(event.Bucket)
    if !shouldRebuildBucket(bucket) {
        return
    }
    
    // 关闭所有控制连接（动态配置随之消失）
    if server.GlobalControlManager != nil {
        server.GlobalControlManager.CloseAll()
    }
    
    // 重建 RuleMatcher（仅静态配置）
    doRebuildMatcher(bucket)
    
    // 反向连接自动重连，重新注入动态配置
}
```

**3. ControlManager 注入动态配置**

```go
// server/control_server.go
func (m *ControlManager) handleRegister(...) {
    // 创建动态 proxy/rule/matcher
    proxy := &config.Proxy{Name: "dyn-proxy-" + hexID, ...}
    rule := fmt.Sprintf("MATCH,%s#%s", proxy.Name, mappingName)
    matcher := config.NewMatchAllMatcher(...)
    
    // 注入到当前 RuleMatcher
    matcher, _ := db.GetMatcher()
    matcher.AddDynamicConfig(proxy, rule, matcher)
    
    // 记录到 session（断开时清理）
    session.DynamicProxy = proxy
    session.DynamicRule = rule
}
```

### 禁止事项

- 禁止将动态配置持久化到数据库
- 禁止在 DB 重载时保留控制连接（会导致动态配置不一致）
- 禁止绕过 `AddDynamicConfig` 直接修改 `RuleMatcher` 内部字段

## 下一步

1. **Phase 1: 运行时状态落库** - 创建proxy_health和subscription_nodes bucket，实现DB操作函数
2. **Phase 2: 修改健康检查和订阅拉取逻辑** - 完成后写入数据库
3. **Phase 3: 消除s.runtime** - 修改所有使用s.runtime的地方，改为查询数据库
4. **Phase 4: 消除启动时全量加载** - 修改main.go启动逻辑
5. **Phase 5: 动态配置融合** - 实现 RuleMatcher 动态配置注入机制
6. **测试验证**：每个阶段迁移后部署测试，确保功能正常

## 时间估算（更新）

- Phase 1（运行时状态落库）：1-2 天
- Phase 2（修改健康检查和订阅拉取）：1 天
- Phase 3（消除s.runtime）：2-3 天
- Phase 4（消除启动时全量加载）：1 天
- 测试验证：2-3 天

**总计：7-10 天**

## Phase 5：运行时状态统一落库

### 背景

当前系统中存在多个运行时状态分散存储在文件系统中，违反了"数据库是唯一事实源"的核心原则。需要审视所有文件操作，将运行时状态迁移到数据库。

### 决策原则

**核心原则：能落库的就落库。** 数据库是唯一事实源，所有运行时状态都应存储在数据库中。

**例外情况（必须保留为文件）**：
- ✅ Watchdog 使用的数据（数据库初始化前需要访问）
- ✅ 大文件（>1MB，数据库不适合存储大 blob）
- ✅ 日志文件（追加写入，自动轮转）

**文件存储（watchdog 使用，DB 初始化前需要访问）**：

| 文件 | 用途 | 理由 |
|------|------|------|
| `data/state/stopped` | 优雅停止标记 | watchdog 在 DB 初始化前读取，判断是否需要回滚到上一个可用版本 |
| `data/state/last-good-worker` | 最后成功的 worker 路径 | watchdog 在 DB 初始化前读取，用于回滚时启动旧版本 |

**数据库存储（非 watchdog 使用）**：

| Key | Value 类型 | 用途 |
|-----|-----------|------|
| `node-id` | string | 全局实例标识（mesh、reverse、admin 共用） |
| `session-secret` | []byte (32B) | Admin 会话密钥（敏感数据） |
| `tls-cert` | []byte | 自动生成的 TLS 证书（敏感数据） |
| `tls-key` | []byte | 自动生成的 TLS 私钥（敏感数据） |
| `dhcp-leases` | []DHCPLease | DHCP 租约列表（运行时状态） |
| `port-bindings` | []PortBinding | 端口绑定列表（运行时状态） |
| `tun-state` | *TUNState | TUN 设备状态（运行时状态） |

### 迁移决策

#### ✅ 已迁移到数据库（10个）

| 原文件 | 数据库 Key | 大小 | 理由 |
|--------|-----------|------|------|
| `data/state/mesh-state.json` | `state/node-id` + `config/mesh` | ~100B | node-id 是全局实例标识，subnet 是 mesh 配置 |
| `data/reverse-id` | `state/node-id` | 32B | 统一使用 node-id，不再单独存储 reverse-id |
| `data/state/dhcp-leases.json` | `state/dhcp-leases` | 几KB | 运行时状态，频繁更新 |
| `data/state/bindings.json` | `state/port-bindings` | 几KB | 运行时状态，频繁更新 |
| `admin-session-secret.key` | `state/session-secret` | 32B | 敏感数据，需要原子更新 |
| `admin-tls-cert.pem` + `admin-tls-key.pem` | `state/tls-cert` + `state/tls-key` | 几KB | 程序自动生成，敏感数据 |
| `data/packages/*.json` | `BucketPackages` | 几KB/个 | 元数据应统一存储，二进制文件保留在文件系统 |

#### ✅ 保留在文件系统（正确设计）

**Watchdog 文件（2个）** - 数据库初始化前需要访问：

| 文件 | 用途 | 理由 |
|------|------|------|
| `data/state/stopped` | 优雅停止标记 | watchdog 在 DB 初始化前读取，判断是否需要回滚 |
| `data/state/last-good-worker` | 最后成功的 worker 路径 | watchdog 在 DB 初始化前读取，用于启动旧版本 |

**大文件（3类）** - 数据库不适合存储大 blob：

| 文件 | 大小 | 理由 |
|------|------|------|
| `packages/*.json` | 每个几KB，总数多 | 包元数据是内容/资产，不是运行时状态 |
| P2P 缓存二进制 | 几MB~几十MB | 大二进制文件 |
| `wintun.dll` | 几MB | 大二进制文件 |

**崩溃恢复文件（1个）** - 必须在数据库损坏时仍可访问：

| 文件 | 位置 | 理由 |
|------|------|------|
| `/var/run/phaethon-tun.state` | tmpfs | 数据库损坏时仍需恢复内核参数 |

**用户管理的凭证** - 用户通过外部工具管理：

| 文件 | 理由 |
|------|------|
| TLS 证书文件（配置指定） | 用户通过 openssl/certbot 生成，配置中的 `tls-cert`/`tls-key`/`client-ca` 只存文件路径（不支持 inline PEM）。优先级：AdminConfig 文件路径 > `state/tls-cert`/`state/tls-key`（自动生成）。用户想替换证书：方式 A）配置 AdminConfig 文件路径；方式 B）直接替换 `state/tls-cert`/`state/tls-key` |
| SSH 私钥（配置指定） | 用户通过 ssh-keygen 生成，配置中的 `private-key` 字段作为文件路径或 inline PEM |

**系统文件** - 只读，OS 管理：

| 文件 | 理由 |
|------|------|
| `/proc/net/route` | 系统只读文件 |
| `/proc/sys/net/ipv4/*` | 系统内核参数 |
| `/etc/resolv.conf` | 系统 DNS 配置 |

**日志文件** - 追加写入，自动轮转：

| 文件 | 理由 |
|------|------|
| `phaethon.log` | 日志文件 |
| `startup-error.log` | 日志文件 |

### 新增 Bucket 设计

| Bucket | Key | Value 类型 | 说明 | 原文件位置 |
|--------|-----|-----------|------|-----------|
| `state` | `node-id` | string | 全局实例标识（mesh、reverse、admin 共用） | `data/state/mesh-state.json` + `data/reverse-id` |
| `state` | `session-secret` | []byte (32B) | Admin 会话密钥（敏感数据） | `admin-session-secret.key` |
| `state` | `tls-cert` | []byte | 自动生成的 TLS 证书（敏感数据） | `admin-tls-cert.pem` |
| `state` | `tls-key` | []byte | 自动生成的 TLS 私钥（敏感数据） | `admin-tls-key.pem` |
| `state` | `dhcp-leases` | []DHCPLease | DHCP 租约列表（运行时状态） | `data/state/dhcp-leases.json` |
| `state` | `port-bindings` | []PortBinding | 端口绑定列表（运行时状态） | `data/state/bindings.json` |
| `state` | `tun-state` | *TUNState | TUN 设备状态（运行时状态） | 新增 |
| `packages` | `{id}` | PackageMeta | 包元数据（二进制文件保留在 `data/packages/*.pkg`） | `data/packages/*.json` |

**注意**：原设计中的 `instance-state` 和 `runtime-state` 两个 bucket 已合并为统一的 `state` bucket，简化 bucket 结构。

### 数据模型

```go
// DHCPLease 存储单个 DHCP 租约
type DHCPLease struct {
    IP        string    `json:"ip"`
    MAC       string    `json:"mac"`
    Hostname  string    `json:"hostname,omitempty"`
    ExpiresAt time.Time `json:"expiresAt"`
}

// PortBinding 存储端口绑定（复用 server.PortBinding 的 13 个字段）
// 已在 db/runtime_state_extended.go 中定义

// TUNState 存储 TUN 设备状态
type TUNState struct {
    Interface string `json:"iface"`
    RPFilter  int    `json:"rp_filter"`
    IPForward int    `json:"ip_forward"`
}
```

**注意**：原设计中的 `MeshIdentity` 结构体已删除。node-id 作为全局实例标识独立存储在 `state/node-id`，subnet 存储在 `config/mesh` 的 MeshConfig 中。

### 迁移策略

**一次性割接**（无向后兼容）：
1. 部署前运行独立迁移工具：`./migrate_data`
2. 迁移工具读取旧文件，写入数据库
3. 输出迁移报告
4. 部署新版本应用（只使用数据库）

**应用代码原则**：
- ❌ 不包含任何 legacy 兼容逻辑
- ❌ 不尝试读取旧文件
- ✅ 只使用数据库
- ✅ 迁移逻辑独立于应用代码

### 实现状态

#### ✅ 已完成

- [x] 添加统一的 `state` bucket（合并原 `instance-state` 和 `runtime-state`）
- [x] 实现 Get/Put 辅助函数
- [x] 添加数据模型定义
- [x] 迁移 mesh-state.json → `state/node-id`（全局实例标识）+ `config/mesh`（subnet）
- [x] 迁移 reverse-id → 统一使用 `state/node-id`（消除冗余身份概念）
- [x] 迁移 dhcp-leases.json → `state/dhcp-leases`
- [x] 迁移 bindings.json → `state/port-bindings`
- [x] 迁移 admin-session-secret.key → `state/session-secret`
- [x] 迁移 admin-tls-cert.pem → `state/tls-cert`
- [x] 迁移 admin-tls-key.pem → `state/tls-key`
- [x] 迁移 data/packages/*.json → BucketPackages（二进制文件保留在文件系统）
- [x] 移除所有 legacy 文件读写代码
- [x] 移除文件路径常量和未使用的 import
- [x] 扩展迁移工具支持所有运行时状态和 Package 元数据
- [x] 删除死代码 cmd/setup/profile.go
- [x] 删除 MeshIdentity 结构体和 GetMeshIdentity/PutMeshIdentity 函数
- [x] 删除 GetReverseID/PutReverseID 函数（统一使用 node-id）
- [x] 保留 stopped-marker 和 last-good-worker 为文件（watchdog 使用）

### 优势

1. **统一数据源**：所有运行时状态都在数据库中，消除不一致风险
2. **简化备份**：只需备份 phaethon.db，不需要分散的状态文件
3. **原子操作**：数据库事务保证状态一致性
4. **性能提升**：mmap 访问比文件系统更快
5. **代码清晰**：消除文件路径拼接和错误处理
6. **无兼容包袱**：应用代码纯净，不包含 legacy 逻辑

### 风险评估

- **低风险**：这些都是小状态文件，迁移简单
- **回滚方案**：迁移工具不删除旧文件，可手动恢复
- **测试策略**：在每个环境验证迁移和读写

