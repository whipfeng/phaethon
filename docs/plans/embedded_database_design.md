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
- ❌ 禁止维护全局的 `activeConf` 或 `res.conf`（各组件应持有自己的缓存或引用）
- ✅ 新增功能应直接查询数据库（`db.GetXxx()`、`db.ListXxx()`）
- ✅ 高频读取场景应设计按需缓存（按 bucket 独立）

## 下一步

1. **Phase A 实施**：识别高频访问模式，实现 ProxyCache、RuleCache
2. **逐步迁移**：从 admin API 开始，逐步替换 `conf.Xxx` 为直接查询
3. **测试验证**：每个组件迁移后部署测试，确保功能正常
4. **性能监控**：对比迁移前后的性能，验证 mmap 性能足够

## 时间估算（更新）

- Phase A（组件级直接查询）：3-5 天
- Phase B（消除 LoadConf）：1-2 天
- Phase C（清理结构体）：1 天
- 测试验证：2-3 天

**总计：7-11 天**
