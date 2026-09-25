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

**数据库是唯一事实源（per-key 细粒度存储）。** 运行时 `*config.RuleConfiguration` 是从数据库派生的工作快照：启动与 OnReload 一律从 DB 装配；每次 API 授权变更在同一临界区内先改内存、再原子落库，两者永不漂移。

### 数据流向

```
启动 / OnReload（读取路径，唯一装配入口）:
  db.LoadRuleConf()
    → 同一只读事务全量读出各 bucket → 组装 RuleConfiguration + Init() 构建索引
    → 后处理（ReverseID、订阅缓存、健康状态、UDP 端口范围）
    → run() 全量重建运行资源（引擎/监听器/P2P 与新 conf 共享指针）

轻量变更（控制台编辑，热更新路径）:
  API 请求 → 校验
    → 原地修改共享 conf + mergeAndInitLocked()   # 引擎即时生效（共享指针）
    → db.ImportRuleConf(conf)                     # 原子落库（持久化）
    → BumpVersion("config")                       # 通知前端刷新
```

### 为什么原地修改而非"重建快照再替换"

- 引擎、监听器、P2P 与 conf **共享同一指针**，原地修改 + `mergeAndInitLocked()`（迁移 SubProxies、组活跃成员等运行时状态后重新 Init）是现有热更新机制，引擎立即生效
- 若每次改动从 DB 重建新对象再替换，引擎仍持旧指针，等于每次改动都做 OnReload 全量重建（重启监听器/引擎），代价不成比例
- 快照的"派生"体现在装配时刻：启动/重载从 DB 读，运行时共享对象只是工作副本

### 禁止事项

- 禁止任何代码绕过 admin API 直接修改运行时配置
- 禁止后台任务将运行时 conf 静默回写 DB（`ImportRuleConf` 只允许出现在 API 授权变更、mesh 自动分配 subnet 等明确持久的场景，且与内存修改同临界区）
- YAML 仅作为导入/导出格式（`/api/config/raw` GET=导出、PUT=导入；首启迁移），不再是运行时配置来源

## 根基设置（Foundational Settings）

### 定义

根基设置是节点身份与访问入口的核心配置，**init 交互过程中确定，reset 不覆盖，但提供单独 API 修改**：

| 根基设置 | 存储位置 | 说明 |
|---------|---------|------|
| Mesh node-id | config bucket, key=mesh | 节点在 mesh 网络中的唯一标识，决定 VIP 与 subnet |
| Mesh subnet | config bucket, key=mesh | 节点管理的 VIP 地址范围 |
| Admin port | config bucket, key=admin | Admin API 监听端口 |
| Admin auth | config bucket, key=admin | Admin API 认证配置（username/password/token） |

### Reset 语义

**Reset 保留根基设置**，只重置其他配置（proxies、rules、mappings、resolvers、subscriptions、reverse-configs、tun、interactive、port-range）：

```
POST /api/config/reset
  → 加载 defaultRaw
  → 从当前 DB 读取 mesh + admin 配置
  → 合并到 newConf（保留根基）
  → db.ImportRuleConf(newConf)
  → OnReload（全量重建）
```

### 根基设置修改 API

```
PUT /api/config/mesh
  → 更新 mesh 配置（node-id、subnet）
  → db.ImportRuleConf(conf)
  → OnReload（重启 mesh 子系统：重新通告、重建路由表）
  → 警告：修改 node-id 会改变节点身份，其他节点需要重新发现

PUT /api/config/admin
  → 更新 admin 配置（port、auth）
  → db.ImportRuleConf(conf)
  → OnReload（重启 admin 服务器：重新绑定端口、更新认证中间件）
  → 警告：修改 port 会改变访问地址
```

**热生效机制**：根基设置修改触发 OnReload（全量重建），因为：
- Mesh 身份变化需要重启 P2P、重建路由表、重新通告
- Admin 端口变化需要重新绑定监听器

OnReload 是现有机制（config reset、YAML 导入都走此路径），代价可接受（根基设置修改频率极低）。

### 禁止事项（补充）

- 禁止 reset 覆盖根基设置（mesh node-id/subnet、admin port/auth）
- 禁止绕过 API 直接修改根基设置（必须通过 PUT /api/config/mesh 或 PUT /api/config/admin）

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
- **批量落库**（`ImportRuleConf`，配置持久化/YAML 导入/配置重置都走此入口）：单个写事务原子替换所有配置 bucket（先清空再写入，**不触碰** fakeip/packages），提交后触发一次合成事件 `{bucket: "config", key: "__rebuilt__", op: "put"}`
- 配置热更新**不依赖** watcher（原地修改共享 conf 即时生效）；变更事件面向后续阶段的订阅者（如 Fake-IP 持久化、审计）与未来的外部集成。合成事件避免批量落库触发 N 次回调

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
? 是否导入现有配置？(y/N): y
? 配置文件路径: ./config.yaml
INFO: 导入配置成功
INFO: 数据库创建完成：phaethon.db
INFO: 启动服务...
```

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

1. 包元数据迁移（文件系统 → 数据库）
2. Fake-IP 映射持久化
3. 配置数据迁移（YAML → 数据库）

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
| 导出/导入 | 整库 | YAML 导出/导入 |

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

## 下一步

1. 确认设计方案
2. 创建任务清单
3. 开始 Phase 1 实现
