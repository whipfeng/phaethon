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

## 数据模型

### 1. 配置数据（Configuration）

#### 1.1 代理配置（Proxies）
```
Key: proxy:{name}
Value: {
  "name": "GG_PROXY",
  "type": "trojan",
  "server": "106.13.183.103",
  "port": 39999,
  "password": "xxx",
  "sni": "www.example.com",
  ...
}
```

#### 1.2 规则配置（Rules）
```
Key: rule:{index}
Value: {
  "type": "DOMAIN-SUFFIX",
  "value": "google.com",
  "proxy": "GG_PROXY",
  "index": 0
}
```

#### 1.3 Mesh 配置
```
Key: config:mesh
Value: {
  "node_id": "auto-generated-xxxxx",
  "subnet": "100.179.0.0/16",
  "vip": "100.179.0.1",
  "enabled": true
}
```

#### 1.4 Admin 配置
```
Key: config:admin
Value: {
  "listen": ":39999",
  "auth": {
    "username": "admin",
    "password": "xxx"
  }
}
```

### 2. 状态数据（State）

#### 2.1 Fake-IP 映射
```
Key: fakeip:domain:{domain}
Value: {
  "ip": "100.179.0.10",
  "domain": "google.com",
  "created_at": "2026-09-24T10:00:00Z",
  "last_used": "2026-09-24T10:05:00Z"
}

Key: fakeip:ip:{ip}
Value: {
  "domain": "google.com",
  "ip": "100.179.0.10"
}
```

### 3. 历史数据（History）

#### 3.1 包元数据
```
Key: pkg:{platform}:{arch}:{version}
Value: {
  "platform": "linux",
  "arch": "amd64",
  "version": "v1.2.3",
  "path": "/path/to/package.pkg",
  "size": 12345678,
  "hash": "sha256:xxxxx",
  "created_at": "2026-09-24T10:00:00Z",
  "published": true
}
```

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
