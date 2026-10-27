# 数据迁移分析：订阅节点和规则

**创建时间**: 2026-10-26  
**背景**: QG 环境数据库损坏，从 Oct 23 08:01 备份恢复后丢失订阅节点和规则数据

---

## 问题根源

### 时间线
1. **Oct 23 08:01**: 数据库备份（正常）
2. **Oct 23 14:58**: commit `2e55b38` - 订阅节点持久化到数据库
3. **Oct 23 20:00**: commit `a7c67f8` - 规则数据结构重构
4. **Oct 26 21:00**: 数据库损坏，从 Oct 23 08:01 备份恢复
5. **结果**: 备份中不包含新格式的订阅节点和规则数据

### 核心问题
- **旧备份**：只有 `config.yaml` 和旧的订阅缓存文件（`data/cache/subscription/*.cache`）
- **新代码**：期望数据在数据库的 `BucketSubscriptionNodes` 和 `BucketRules` 中
- **缺失**：备份恢复后，数据库中没有新格式的数据

---

## 数据结构对比

### 1. 订阅节点（Subscription Nodes）

#### 旧格式（Oct 23 14:58 之前）
**存储位置**: 文件系统  
**路径**: `data/cache/subscription/<subscription-name>.cache`  
**格式**: Clash YAML 或 Base64 编码的 URI 列表

**示例文件内容** (`Starlink-01.cache`):
```yaml
mode: rule
profile:
  store-selected: true
  fake-ip: true
proxies:
- { name: 🇸🇬 新加坡1, type: hysteria2, server: starlink-sg1.6send.com, password: xxx, port: 443, ... }
- { name: 🇺🇸 美国1, type: trojan, server: xxx, password: xxx, port: 443, ... }
```

**加载流程**（旧代码 `main.go`）:
```go
subCacheDir = filepath.Join(dataDir, "cache", "subscription")
for _, sub := range conf.Subscriptions {
    cached, err := config.LoadSubscriptionCache(subCacheDir, sub.Name)
    if err != nil || cached == "" {
        continue
    }
    proxies, err := config.ParseSubscription(cached)
    if err != nil {
        continue
    }
    sub.SetNodes(proxies)  // 设置到内存中的 sub.SubProxies
}
```

**特点**:
- ✅ 每次启动都从文件读取
- ✅ 网络获取失败时回退到文件缓存
- ❌ 没有持久化到数据库，重启后需要重新解析
- ❌ `sub.SubProxies` 是运行时字段（`yaml:"-" json:"-"`），不序列化

#### 新格式（Oct 23 14:58 之后）
**存储位置**: 数据库  
**Bucket**: `BucketSubscriptionNodes`  
**Key**: 订阅名称（如 "Starlink-01"）  
**Value**: JSON 序列化的 `SubscriptionNodes` 结构

**数据结构** (`db/runtime_state.go`):
```go
type SubscriptionNodes struct {
    SubscriptionName string                `json:"subscription_name"`
    Proxies          map[string]*config.Proxy `json:"proxies"`
    LastUpdate       time.Time             `json:"last_update"`
}
```

**存储流程**（新代码 `main.go` `refreshSubscription`）:
```go
sub.SetNodes(proxies)
// 新增：保存到数据库
if err := db.PutSubscriptionNodes(sub.Name, sub.SubProxies); err != nil {
    util.LogModuleWarn("system", "persist subscription nodes failed: %v", err)
}
```

**加载流程**（新代码 `db/matcher.go` `BuildRuleMatcher`）:
```go
for _, sub := range subs {
    nodes, err := GetSubscriptionNodes(sub.Name)  // 从数据库读取
    if err != nil {
        return fmt.Errorf("load subscription nodes for %s: %w", sub.Name, err)
    }
    if nodes != nil {
        sub.SubMu.Lock()
        sub.SubProxies = nodes  // 设置到内存
        sub.SubMu.Unlock()
    }
}
```

**特点**:
- ✅ 持久化到数据库，启动时直接加载
- ✅ 不需要重新解析 YAML/URI
- ✅ 与订阅元数据（`BucketSubscriptions`）分离
- ❌ 旧备份中没有这些数据

---

### 2. 规则（Rules）

#### 旧格式（Oct 23 20:00 之前）
**存储位置**: `config.yaml` 文件  
**结构**: `RuleConfiguration.Rules []string`

**示例** (`config.yaml`):
```yaml
rules:
- DOMAIN-SUFFIX,google.com,Proxy
- DOMAIN-KEYWORD,facebook,Proxy
- IP-CIDR,192.168.0.0/16,DIRECT
- GEOIP,CN,DIRECT
- MATCH,Final
```

**加载流程**（旧代码 `config/config.go`）:
```go
data, err := os.ReadFile(configPath)
yaml.Unmarshal(data, &conf)
// conf.Rules 是 []string，每个元素是 "TYPE,pattern,proxy"
```

**特点**:
- ✅ 人类可读的 YAML 格式
- ❌ 每次修改需要重写整个配置文件
- ❌ 无法原子操作（增删改查）

#### 新格式（Oct 23 20:00 之后）
**存储位置**: 数据库  
**Bucket**: `BucketRules`  
**Key**: 规则索引（如 "0", "1", "2"）  
**Value**: 规则字符串（如 "DOMAIN-SUFFIX,google.com,Proxy"）

**数据结构**（`db/configstore.go`）:
```go
// 存储
func PutRule(index int, rule string) error {
    key := fmt.Sprintf("%d", index)
    return Put(BucketRules, []byte(key), rule)
}

// 加载
func listRulesTx(tx *bolt.Tx) ([]string, error) {
    b := tx.Bucket(BucketRules)
    var rules []string
    err := forEachTx(b, func(k, v []byte) error {
        var rule string
        if err := json.Unmarshal(v, &rule); err != nil {
            return err
        }
        rules = append(rules, rule)
        return nil
    })
    return rules, nil
}
```

**加载流程**（新代码 `db/matcher.go` `BuildRuleMatcher`）:
```go
ruleStrs, err := listRulesTx(tx)
if err != nil {
    return fmt.Errorf("load rules: %w", err)
}
m.rules = parseRules(ruleStrs)  // 解析为 config.Rule 对象
```

**特点**:
- ✅ 支持精确增删改查（`PutRule`, `DeleteRule`, `ListRules`）
- ✅ 原子操作，不需要重写整个配置
- ✅ 与代理、订阅等数据分离
- ❌ 旧备份中没有这些数据（在 `config.yaml` 中）

---

## 迁移方案

### 迁移源（FROM）
1. **订阅节点**: `data/cache/subscription/*.cache` 文件（Clash YAML 或 URI 列表）
2. **规则**: `config.yaml` 文件中的 `rules:` 字段（如果存在）

### 迁移目标（TO）
1. **订阅节点**: 数据库 `BucketSubscriptionNodes`，key=订阅名，value=`SubscriptionNodes` JSON
2. **规则**: 数据库 `BucketRules`，key=索引，value=规则字符串 JSON

### 迁移工具设计

#### 工具位置
`tools/migrate_data.go` — 独立的一次性迁移工具，**不侵入主应用**

#### 迁移流程

**Step 1: 迁移订阅节点**
```go
for each .cache file in data/cache/subscription/:
    1. 读取文件内容（Clash YAML 或 URI 列表）
    2. 调用 config.ParseSubscription() 解析为 []*config.Proxy
    3. 转换为 map[string]*config.Proxy（key=proxy.Name）
    4. 调用 db.PutSubscriptionNodes(subName, proxies) 写入数据库
    5. 日志记录：迁移成功/失败
```

**Step 2: 迁移规则**（如果 config.yaml 存在）
```go
if config.yaml exists:
    1. 读取 config.yaml
    2. 解析 YAML 提取 rules 字段
    3. for each rule in rules:
        a. 调用 db.PutRule(index, rule) 写入数据库
        b. 日志记录：迁移成功/失败
```

**Step 3: 验证**
```go
1. 从数据库读取所有订阅节点
2. 验证数量与源文件一致
3. 从数据库读取所有规则
4. 验证数量与源文件一致
5. 输出迁移报告
```

#### 工具特性
- ✅ **独立运行**：不修改主应用代码，不影响正常启动流程
- ✅ **幂等性**：可以多次运行，重复写入相同数据不会出错
- ✅ **只读源**：只读取旧文件，不删除或修改
- ✅ **详细日志**：每个步骤都有日志输出，便于排查问题
- ✅ **错误处理**：单个订阅/规则失败不影响其他数据迁移
- ✅ **验证机制**：迁移后自动验证数据完整性

---

## 执行计划

### 阶段 1：开发迁移工具
1. 创建 `tools/migrate_data.go`
2. 实现订阅节点迁移逻辑
3. 实现规则迁移逻辑（可选，取决于 config.yaml 是否存在）
4. 添加验证和日志

### 阶段 2：本地测试
1. 使用本地测试数据验证工具
2. 确认迁移逻辑正确
3. 确认不影响主应用

### 阶段 3：部署到 QG
1. 编译迁移工具：`go build -o migrate_data tools/migrate_data.go`
2. 上传到 QG：`scp migrate_data root@10.11.61.50:/root/`
3. 停止 phaethon 服务（避免数据库冲突）
4. 运行迁移工具：`./migrate_data`
5. 验证迁移结果
6. 启动 phaethon 服务

### 阶段 4：验证
1. 检查旁路网关是否恢复
2. 检查订阅节点是否显示在控制台
3. 检查规则是否生效
4. 测试代理功能

---

## 关键代码位置

### 旧代码（迁移源）
| 功能 | 文件 | 函数 | 说明 |
|------|------|------|------|
| 读取订阅缓存 | `config/subscription.go` | `LoadSubscriptionCache()` | 从文件读取 |
| 解析订阅内容 | `config/subscription.go` | `ParseSubscription()` | YAML/URI → []*Proxy |
| 读取配置 | `config/config.go` | `LoadConfig()` | 读取 config.yaml |

### 新代码（迁移目标）
| 功能 | 文件 | 函数 | 说明 |
|------|------|------|------|
| 写入订阅节点 | `db/runtime_state.go` | `PutSubscriptionNodes()` | 写入数据库 |
| 写入规则 | `db/configstore.go` | `PutRule()` | 写入数据库 |
| 读取订阅节点 | `db/runtime_state.go` | `GetSubscriptionNodes()` | 从数据库读取 |
| 读取规则 | `db/configstore.go` | `listRulesTx()` | 从数据库读取 |

---

## 风险评估

### 低风险
- ✅ 迁移工具独立运行，不影响主应用
- ✅ 只读源文件，不删除或修改
- ✅ 幂等性，可以多次运行
- ✅ 详细日志，便于排查

### 中风险
- ⚠️ 数据库操作需要停止 phaethon 服务（避免冲突）
- ⚠️ 旧文件格式可能不标准（需要容错处理）

### 缓解措施
- 迁移前备份数据库
- 迁移时详细日志
- 迁移后验证数据完整性
- 失败时回滚到迁移前状态

---

## 总结

**问题本质**：数据库备份时间点早于数据持久化功能上线，导致新格式数据缺失

**解决方案**：
1. 从旧文件（`data/cache/subscription/*.cache`）恢复订阅节点
2. 从旧配置（`config.yaml`）恢复规则（如果存在）
3. 使用独立迁移工具，不侵入主应用
4. 迁移后验证数据完整性

**预期结果**：旁路网关恢复正常，订阅节点和规则数据完整
