# 规则管理：稳定 ID 设计（v2）

**创建时间**: 2026-10-27  
**状态**: 设计中  
**优先级**: 高

## 问题背景

当前规则管理基于 index（数组下标）进行操作，存在并发问题：

1. **Index 不稳定**：删除/插入规则后，后续规则的 index 会变化
2. **并发冲突**：两个客户端同时操作时，一个客户端的删除会导致另一个客户端的 index 失效
3. **用户体验差**：前端需要维护 index 映射，容易出错
4. **效率低**：每次操作都需要读写整个数组

## 设计目标

1. **稳定标识**：每条规则有唯一的、稳定的自增 ID
2. **并发安全**：基于 ID 的操作不受其他规则增删的影响
3. **细粒度操作**：每条规则独立存储，操作单条规则不影响其他规则
4. **显式排序**：顺序通过 ID 列表维护，与规则数据分离
5. **一次性割接**：提供迁移工具，不做自动格式转换

## 数据库 Schema

### 当前设计（废弃）

```go
// BoltDB Bucket: rules
// Key: "rules"
// Value: ["rule1", "rule2", "rule3"]  // 纯字符串数组
```

### 新设计

```go
// BoltDB Bucket: rules
// Key: "next_id"        → 123              // 自增序列（uint64）
// Key: "order"          → [1, 5, 3, 8, 2]  // 规则 ID 的有序列表
// Key: "rule:1"         → "DOMAIN-SUFFIX,google.com,PROXY"
// Key: "rule:5"         → "IP-CIDR,192.168.1.0/24,DIRECT"
// Key: "rule:3"         → "MATCH,PROXY"
// ...
```

**核心思想：**
- 每条规则独立存储（`rule:{id}` → 规则内容）
- 顺序通过 `order` 列表维护（只存 ID，不存规则内容）
- `next_id` 提供自增 ID 生成

**优势：**
1. **真正的细粒度操作**：更新/删除单条规则只操作一个 key
2. **排序与数据分离**：调整顺序只需修改 `order` 列表
3. **高效**：不需要读写整个数组
4. **简单**：自增 ID 比 UUID 更简单可控

## API 设计

### GET /api/rules

**响应**：
```json
[
  {"id": 1, "rule": "DOMAIN-SUFFIX,google.com,PROXY", "enabled": true},
  {"id": 5, "rule": "IP-CIDR,192.168.1.0/24,DIRECT", "enabled": false},
  {"id": 3, "rule": "MATCH,PROXY", "enabled": true}
]
```

**实现**：读取 `order` 列表，然后逐个读取 `rule:{id}`，组装返回。

### POST /api/rules（插入规则）

**请求体**：
```json
{
  "rule": "DOMAIN-SUFFIX,example.com,PROXY",
  "after_id": 5  // 可选：插入到 ID=5 之后；null=插入到开头；省略=追加到末尾
}
```

**实现**：
1. 读取并递增 `next_id` → 得到新 ID（如 8）
2. 写入 `rule:8` → 规则内容
3. 修改 `order` 列表：在 ID=5 之后插入 8 → `[1, 5, 8, 3, 2]`
4. 触发版本通知

**响应**：
```json
{
  "status": "inserted",
  "id": 8,
  "position": 2
}
```

### PUT /api/rules/{id}（更新规则）

**请求体**：
```json
{
  "rule": "DOMAIN-SUFFIX,new-example.com,PROXY"
}
```

**实现**：
1. 检查 `rule:{id}` 是否存在
2. 写入 `rule:{id}` → 新规则内容（只修改这一个 key）
3. 触发版本通知

**响应**：
```json
{
  "status": "updated",
  "id": 8
}
```

**错误**：
- 404: ID 不存在

### DELETE /api/rules/{id}（删除规则）

**实现**：
1. 检查 `rule:{id}` 是否存在
2. 删除 `rule:{id}`（只删除这一个 key）
3. 从 `order` 列表中移除该 ID
4. 触发版本通知

**响应**：
```json
{
  "status": "deleted",
  "id": 8
}
```

**错误**：
- 404: ID 不存在

### PATCH /api/rules/{id}（切换启用状态）

**请求体**：
```json
{
  "enabled": true
}
```

**实现**：
1. 读取 `rule:{id}`
2. 修改规则的启用/禁用注释（`//` 前缀）
3. 写回 `rule:{id}`（只修改这一个 key）
4. 触发版本通知

**响应**：
```json
{
  "status": "updated",
  "id": 1,
  "enabled": true
}
```

### POST /api/rules/reorder（批量调整顺序）

**请求体**：
```json
{
  "order": [5, 1, 3, 8, 2]  // 新的 ID 顺序
}
```

**实现**：
1. 验证所有 ID 都存在
2. 直接替换 `order` 列表
3. 触发版本通知

**响应**：
```json
{
  "status": "reordered"
}
```

## 前端变更

### rules.html

1. **表格行使用 data-id**：
```html
<tr data-id="1" class="...">
  <td>1</td>
  <td>DOMAIN-SUFFIX,google.com,PROXY</td>
  <td>
    <label class="switch">
      <input type="checkbox" checked onchange="toggleRule(1, this.checked)">
      <span class="slider"></span>
    </label>
  </td>
  <td>
    <button onclick="editRule(1)">Edit</button>
    <button onclick="deleteRule(1)">Delete</button>
  </td>
</tr>
```

2. **所有操作函数改为接收 ID**：
```javascript
async function deleteRule(id) {
    const res = await fetch(`./api/rules/${id}`, {method: 'DELETE'});
    // ...
}

async function editRule(id) {
    document.getElementById('rule-edit-id').value = id;
    // ...
}

async function saveRule(e) {
    const editId = parseInt(document.getElementById('rule-edit-id').value);
    if (editId > 0) {
        // 更新：PUT /api/rules/{id}
        res = await fetch(`./api/rules/${editId}`, {
            method: 'PUT',
            body: JSON.stringify({rule: ruleStr})
        });
    } else {
        // 插入：POST /api/rules
        res = await fetch('./api/rules', {
            method: 'POST',
            body: JSON.stringify({rule: ruleStr, after_id: insertAfterId})
        });
    }
}
```

3. **全局变量改为 Map**：
```javascript
// 旧：var rules = [...];  // 数组
// 新：
var rulesMap = {};  // {id: rule}
var rulesOrder = [];  // [1, 5, 3, 8, 2]  // 维护顺序
```

4. **SSE 更新函数**：
```javascript
function updateRules(data) {
    rulesMap = {};
    rulesOrder = [];
    data.forEach(r => {
        rulesMap[r.id] = r.rule;
        rulesOrder.push(r.id);
    });
    // 渲染表格...
}
```

## 数据迁移（一次性割接）

### 迁移工具

创建 `tools/migrate_rules_to_ids.go`：

```go
package main

import (
    "fmt"
    "phaethon/db"
)

func main() {
    // 1. 读取旧格式
    oldRules, err := db.ListRulesOld()  // []string
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("Found %d old rules, migrating...\n", len(oldRules))
    
    // 2. 初始化新格式
    var order []uint64
    var nextID uint64 = 1
    
    for _, rule := range oldRules {
        id := nextID
        nextID++
        
        // 写入 rule:{id}
        if err := db.PutRuleByID(id, rule); err != nil {
            panic(err)
        }
        
        order = append(order, id)
        fmt.Printf("  Migrated rule %d: %s\n", id, rule)
    }
    
    // 3. 写入 order 和 next_id
    if err := db.PutRuleOrder(order); err != nil {
        panic(err)
    }
    if err := db.PutNextID(nextID); err != nil {
        panic(err)
    }
    
    // 4. 删除旧 key
    if err := db.DeleteOldRulesKey(); err != nil {
        panic(err)
    }
    
    fmt.Printf("Migration complete! Next ID: %d\n", nextID)
    
    // 5. 验证
    verify()
}

func verify() {
    rules, err := db.ListRules()
    if err != nil {
        panic(err)
    }
    fmt.Printf("Verification: %d rules in new format\n", len(rules))
    for _, r := range rules {
        fmt.Printf("  ID=%d: %s\n", r.ID, r.Rule)
    }
}
```

### 迁移步骤

1. **停止服务**：确保没有并发写入
2. **备份数据库**：`cp phaethon.db phaethon.db.bak`
3. **运行迁移工具**：`./migrate_rules_to_ids`
4. **验证结果**：检查输出是否正确
5. **启动服务**：使用新版本二进制

### 回滚方案

如果迁移失败：
1. 停止服务
2. 恢复备份：`mv phaethon.db.bak phaethon.db`
3. 使用旧版本二进制启动

### 注意事项

- **不做自动格式转换**：旧版本代码无法读取新格式，必须一次性割接
- **迁移前必须备份**：防止数据丢失
- **迁移后验证**：确保所有规则都正确迁移

## 实现计划

### 阶段 1：数据库层（新增函数，不影响现有功能）

1. 定义新的 key 常量：`KeyNextID`, `KeyOrder`, `RuleKeyPrefix`
2. 实现底层函数：
   - `getNextID() / putNextID()` - 自增 ID 管理
   - `getRuleOrder() / putRuleOrder()` - 顺序列表管理
   - `getRuleByID() / putRuleByID() / deleteRuleByID()` - 单条规则 CRUD
3. 实现高层函数：
   - `ListRules()` - 读取 order，逐个读取规则，返回有序列表
   - `InsertRule()` - 分配 ID，写入规则，更新 order
   - `UpdateRule()` - 更新单条规则
   - `DeleteRule()` - 删除规则，更新 order
4. 保留旧函数 `ListRulesOld()` 供迁移工具使用
5. 编写单元测试

### 阶段 2：API 层（破坏性变更）

1. 修改 `GET /api/rules` 返回带 ID 的格式
2. 修改 `POST /api/rules` 支持 `after_id` 参数，返回新 ID
3. 修改 `PUT /api/rules/{id}` 基于 ID 更新（只修改单个 key）
4. 修改 `DELETE /api/rules/{id}` 基于 ID 删除（只删除单个 key）
5. 修改 `PATCH /api/rules/{id}` 基于 ID 切换
6. 新增 `POST /api/rules/reorder` 批量调整顺序

### 阶段 3：前端（配合 API）

1. 修改 `rules.html` 使用 `data-id`（数字 ID）
2. 修改所有 JavaScript 函数使用 ID
3. 修改 `app.js` 的 `updateRules()` 函数
4. 测试所有操作（增删改查、启用/禁用、插入位置、拖拽排序）

### 阶段 4：迁移工具

1. 实现 `tools/migrate_rules_to_ids.go`
2. 编译迁移工具
3. 在测试环境验证迁移流程
4. 准备回滚方案

### 阶段 5：部署割接

1. 停止 QG 服务
2. 备份数据库
3. 运行迁移工具
4. 验证迁移结果
5. 部署新版本二进制
6. 启动服务
7. 验证功能正常
8. 部署到其他环境

## 风险与缓解

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| 迁移失败 | 规则丢失 | 迁移前备份；准备回滚方案 |
| 数据不一致 | order 与实际规则不匹配 | 迁移后验证；事务保证一致性 |
| API 不兼容 | 前端报错 | 前后端同时部署 |
| 性能问题 | 读取 order 后逐个读取规则 | BoltDB 读取很快；规则数量有限 |
| 并发冲突 | 两个客户端同时修改 order | 服务器端 mutex 串行化 |

## 测试用例

1. **基础操作**：
   - 添加规则到末尾
   - 添加规则到指定位置（after_id）
   - 添加规则到开头（after_id=null）
   - 更新规则（只修改单个 key）
   - 删除规则（只删除单个 key）
   - 切换启用/禁用
   - 批量调整顺序

2. **并发场景**：
   - 两个客户端同时删除不同规则
   - 一个客户端删除，另一个客户端更新
   - 多个客户端同时添加规则
   - 验证 ID 稳定性（删除不影响其他规则的 ID）

3. **迁移场景**：
   - 旧格式数据迁移到新格式
   - 迁移后功能正常
   - 迁移失败回滚

4. **边界情况**：
   - 空规则列表
   - 删除不存在的 ID（404）
   - 更新不存在的 ID（404）
   - 插入到不存在的 ID 之后（400）
   - order 列表与实际规则不一致

5. **性能测试**：
   - 1000 条规则的列表查询
   - 1000 条规则的批量删除
   - 1000 条规则的顺序调整

## 验收标准

- [ ] 每条规则有唯一的自增 ID
- [ ] 规则独立存储（`rule:{id}` → 内容）
- [ ] 顺序通过 `order` 列表维护
- [ ] 所有 API 基于 ID 操作
- [ ] 更新/删除操作只影响单个 key
- [ ] 并发操作不会导致 ID 错乱
- [ ] 迁移工具正确转换旧数据
- [ ] 前端所有功能正常
- [ ] 单元测试覆盖率 > 80%
- [ ] QG 环境割接成功
- [ ] 性能无明显下降

## 设计优势总结

1. **真正的细粒度操作**：更新/删除单条规则只操作一个 key，不影响其他规则
2. **显式排序**：顺序与数据分离，调整顺序只需修改 `order` 列表
3. **并发安全**：基于 ID 的操作不受其他规则增删的影响
4. **高效**：不需要读写整个数组，只操作需要的 key
5. **简单可控**：自增 ID 比 UUID 更简单，易于调试
6. **易于扩展**：可以为每条规则添加更多元数据（如创建时间、修改时间等）

## 参考资料

- RESTful API 设计最佳实践
- UUID v4 规范（RFC 4122）
- BoltDB 事务模型
