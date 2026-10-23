# 数据库数据丢失问题

**创建时间**: 2026-10-23  
**状态**: 修复中  
**优先级**: 高（P0）

## 问题描述

VM 环境（10.21.20.65）数据库中的所有配置数据丢失：
- proxies: 0 条（应有多个代理配置）
- proxy_groups: 0 条
- rules: 0 条
- mappings: 0 条
- 其他配置项全部为空

程序启动时日志显示 `total proxies: 0`，确认数据库已空。

## 影响范围

- **VM 环境**：数据丢失，需要恢复
- **QG/JF/GG 环境**：数据完整（已备份）
  - QG: `/root/phaethon.db.backup-20261023-080148` (2.0MB)
  - JF: `/home/layer4/layer4-deploy/phaethon.db.backup-20261023-080148` (1.0MB)
  - GG: `/home/layer4/phaethon-gg/phaethon.db.backup-20261023-080348` (64KB)

## 根因分析

### 问题代码

`db/configstore.go` 中的 `ImportRuleConf()` 函数（旧实现）：

```go
func ImportRuleConf(conf *config.RuleConfiguration) error {
    err := Update(func(tx *bolt.Tx) error {
        // 整体替换语义：先清空配置 bucket
        for _, bucket := range configBuckets() {
            if err := tx.DeleteBucket(bucket); err != nil {
                return fmt.Errorf("清空 bucket %s 失败: %w", string(bucket), err)
            }
            if _, err := tx.CreateBucket(bucket); err != nil {
                return fmt.Errorf("重建 bucket %s 失败: %w", string(bucket), err)
            }
        }
        // ... 然后写入 conf 数据
    })
}
```

### 设计缺陷

1. **全量操作模式**：每次调用都"先 DeleteBucket 再 CreateBucket"，然后重写所有数据
2. **危险的调用链**：`saveConfig()` → `ImportRuleConf()`，每次 API 操作都会触发
3. **数据丢失风险**：如果 `s.conf` 在某个时刻是空的或不完整的，数据就会丢失

### 调用点分析

`ImportRuleConf` 被调用的位置：

1. **admin/admin.go:5720** - `saveConfig()` 每次保存配置时调用 ⚠️ **最危险**
2. **admin/admin.go:5733** - `saveConfig_locked()` 每次保存配置时调用 ⚠️ **最危险**
3. **admin/admin.go:1936** - 配置初始化 API
4. **admin/admin.go:2035** - 配置重置 API（调用 `ResetConfigPreserving`）
5. **main.go:320** - 启动时 mesh 迁移后
6. **main.go:351** - 启动时 subnet 分配后
7. **db/init.go:111** - `ImportYAML()` 从配置文件导入

`saveConfig()` 被调用的位置：**40+ 次**（每个 API 操作都会调用）

### 证据链

1. VM 日志显示 10月19日有 3 次 "config persisted to database" 记录
2. VM 当前数据库 bucket 结构完整但所有数据为空
3. 其他环境数据库完整（未执行过配置保存操作）
4. 代码分析确认 `ImportRuleConf()` 会清空所有 bucket

## 修复方案

### 核心思路

**彻底废除全量操作模式和 s.conf 中间对象，改为精确的 bucket+key 粒度增删改查**

### 阶段 1：紧急修复（已完成）

废除"先清空再重写"的危险模式，改为 diff-based 精确同步：

1. **删除危险函数**：
   - `ImportRuleConf()` - 已废除
   - `ResetConfigPreserving()` - 已废除
   - `ImportYAML()` - 已废除

2. **新增安全函数**：
   - `SyncConfigToDB(conf)` - diff-based 精确同步（临时方案）

3. **重构调用点**：
   - `saveConfig/saveConfig_locked` → 调用 `SyncConfigToDB`
   - `main.go` 启动逻辑 → 直接调用 `PutMeshConfig`
   - 配置初始化 API → 调用 `SyncConfigToDB`
   - 配置重置 API → 调用 `SyncConfigToDB`（保留 mesh/admin）

### 阶段 2：彻底重构（已完成）

废除 `s.conf` 中间对象和 `RuleConfiguration` 结构，所有组件直接查询数据库：

1. **已废除**：
   - `SyncConfigToDB` - 已删除
   - `saveConfig` / `saveConfig_locked` - 已删除
   - `LoadRuleConf()` - 已删除（不再全量加载到内存结构）
   - `config.RuleConfiguration` - 标记为废弃，不再使用

2. **已重构**：
   - 所有 API handler 直接使用 `db.PutXxx()` / `db.DeleteXxx()` / `db.ListXxx()`
   - Admin 服务器启动时直接调用 `db.GetAdminConfig()`
   - Mesh 子系统启动时直接调用 `db.GetMeshConfig()`
   - 各组件按需查询数据库，不再依赖统一的 ruleConf 结构

3. **缓存设计**（按需）：
   - 如某 bucket 数据被频繁读取，可为该 bucket 单独设计缓存
   - 缓存按 bucket 独立，不是全量缓存
   - 提供 Invalidate() 方法，API 修改后调用

### 安全保障

1. **完整性检查**：数据库操作前验证参数
2. **事务原子性**：所有操作在单个事务内完成
3. **细粒度操作**：逐个 key 增删改查，避免"先清空再重写"的中间态
4. **数据库为唯一真实源**：不存在内存中的冗余副本

## 修复进度

- [x] 备份所有环境数据库
- [x] 分析问题根因
- [x] 删除 `ImportRuleConf`、`ResetConfigPreserving`、`ImportYAML`
- [x] 新增细粒度数据库操作（`PutXxx`/`DeleteXxx`/`ListXxx`）
- [x] 重构所有 API handler 使用细粒度操作
- [x] 删除批量导入/重置 API（`POST /api/config`、`PUT /api/config/raw`、`POST /api/config/reset`）
- [x] 删除 `fullConfigImport` 和 `ReplaceAllConfig` 函数
- [x] 删除 `SyncConfigToDB` 和相关辅助函数
- [x] 删除 `LoadRuleConf()` 和 `RuleConfiguration` 结构
- [x] 更新设计文档反映新架构
- [x] 编译验证
- [ ] 修复 VM admin 服务端口问题（数据库 AdminConfig.Addr 可能为空）
- [ ] 部署 VM 环境测试
- [ ] 恢复 VM 数据库数据
- [ ] 部署到其他环境

## 经验教训

1. **禁止全量操作**：数据库操作必须落到 bucket+key 粒度，不允许"先清空再重写"
2. **区分语义**：
   - "从配置文件导入"应该只在第一次初始化时调用
   - "保存配置到数据库"应该用精确的增删改查
3. **安全检查**：任何可能清空数据的操作都必须有完整性验证
4. **事务保护**：使用事务保证原子性，避免中间态导致数据丢失

## 更新日志

- 2026-10-23: 发现问题，完成根因分析，开始修复
