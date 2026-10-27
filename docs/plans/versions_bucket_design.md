# 统一版本号存储方案设计

**创建时间**: 2026-10-27  
**状态**: 设计中  
**优先级**: 高

## 1. 背景

当前版本通知机制存在以下问题：
- 版本号分散在内存中，重启后丢失
- 需要监听所有 bucket 的变化，复杂度高
- bootTime 单独记录，与版本号管理不统一

## 2. 设计目标

1. **统一存储**：所有版本号集中存储在 `versions` bucket
2. **持久化**：版本号持久化到数据库，重启后保持连续
3. **简化监听**：只需监听一个 bucket 的变化
4. **增量通知**：版本号变化时只发送变化的 topic
5. **全量兜底**：定期心跳发送完整 vector，防止错过更新

## 3. Bucket 设计

### 3.1 新增 Bucket

```go
BucketVersions = []byte("versions")  // 统一版本号存储
```

### 3.2 存储格式

```
versions bucket:
  "boot"          → uint64 (启动版本号，每次启动递增)
  "mesh"          → uint64 (mesh 拓扑版本)
  "logs"          → uint64 (连接日志版本)
  "tun"           → uint64 (TUN 状态版本)
  "p2p"           → uint64 (P2P 状态版本)
  "proxies"       → uint64 (代理配置版本)
  "rules"         → uint64 (规则版本)
  "mappings"      → uint64 (映射版本)
  "subscriptions" → uint64 (订阅版本)
  "proxy-groups"  → uint64 (代理组版本)
  "admin-config"  → uint64 (管理配置版本)
  "tun-config"    → uint64 (TUN 配置版本)
  "mesh-config"   → uint64 (mesh 配置版本)
  ... 其他 topic
```

## 4. 核心操作

### 4.1 递增版本号（先读后写，事务内原子）

```go
func incrementVersion(topic string) error {
    return Update(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(BucketVersions)
        
        // 读
        var version uint64
        if data := bucket.Get([]byte(topic)); data != nil {
            version = binary.LittleEndian.Uint64(data)
        }
        
        // 写
        version++
        buf := make([]byte, 8)
        binary.LittleEndian.PutUint64(buf, version)
        return bucket.Put([]byte(topic), buf)
    })
}
```

### 4.2 读取版本号

```go
func readVersion(topic string) uint64 {
    version, err := getView[uint64](BucketVersions, []byte(topic))
    if err != nil || version == nil {
        return 0
    }
    return *version
}
```

### 4.3 读取所有版本号

```go
func readAllVersions() map[string]uint64 {
    result := make(map[string]uint64)
    _ = View(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(BucketVersions)
        if bucket == nil {
            return nil
        }
        return bucket.ForEach(func(k, v []byte) error {
            if len(v) == 8 {
                result[string(k)] = binary.LittleEndian.Uint64(v)
            }
            return nil
        })
    })
    return result
}
```

## 5. 初始化流程

```go
func InitVersionNotifier() {
    // 1. 更新 boot 版本号（标识服务器启动）
    incrementVersion("boot")
    
    // 2. 订阅 versions bucket 变化
    Subscribe(BucketVersions, func(event ChangeEvent) {
        topic := string(event.Key)
        version := readVersion(topic)
        
        // 立即通知 SSE 客户端（增量更新）
        data, _ := json.Marshal(map[string]uint64{topic: version})
        broadcastSSE("heartbeat", data)
    })
    
    // 3. 启动定期心跳（全量同步）
    startPeriodicHeartbeat(30 * time.Second)
}
```

## 6. 数据变化触发

### 6.1 配置变化（自动触发）

```go
// db.Put/Delete 时自动更新对应 bucket 的版本号
func Put(bucket, key, value) error {
    // 写入数据
    tx.Bucket(bucket).Put(key, value)
    
    // 更新版本号
    if shouldNotify(bucket) {
        incrementVersion(string(bucket))
    }
}
```

### 6.2 运行时状态（手动触发）

```go
// mesh 拓扑变化
mesh.recomputeRoutes()
incrementVersion("mesh")

// 连接日志更新
connlog.AddLog(entry)
incrementVersion("logs")

// TUN 状态变化
tun.UpdateStatus(status)
incrementVersion("tun")
```

## 7. SSE 通知机制

### 7.1 增量通知（版本号变化时）

```go
// BumpVersion 时立即发送变化的 topic
func (n *VersionNotifier) BumpVersion(typ string) uint64 {
    incrementVersion(typ)
    version := readVersion(typ)
    
    // 只发送 { "typ": version }
    data, _ := json.Marshal(map[string]uint64{typ: version})
    n.broadcast("heartbeat", data, conns)
    
    return version
}
```

### 7.2 全量心跳（定期发送）

```go
// 定期发送完整 vector，兜底同步
func (n *VersionNotifier) startPeriodicHeartbeat(interval time.Duration) {
    ticker := time.NewTicker(interval)
    go func() {
        for {
            select {
            case <-ticker.C:
                // 发送完整 vector
                data, _ := json.Marshal(readAllVersions())
                n.broadcast("heartbeat", data, conns)
            case <-n.heartbeatStop:
                return
            }
        }
    }()
}
```

## 8. 前端处理逻辑

### 8.1 初始化

```javascript
// 建立 SSE 连接，不发送初始数据
const sseSource = new EventSource('./api/events');

// 等待版本号变化通知
let topicVersions = {};
```

### 8.2 接收心跳

```javascript
sseSource.addEventListener('heartbeat', (e) => {
    const update = JSON.parse(e.data);
    
    // 可能是增量更新 { "proxies": 790 }
    // 也可能是全量 vector { "boot": 123, "mesh": 456, ... }
    // 都能正确处理
    
    for (const [topic, version] of Object.entries(update)) {
        if (topicVersions[topic] !== version) {
            topicVersions[topic] = version;
            dispatchTopic(topic);  // 触发该 topic 的 handler
        }
    }
});
```

### 8.3 轮询兜底

```javascript
// SSE 断开时，定期轮询 /api/versions
async function pollVersions() {
    const res = await fetch('./api/versions');
    const vector = await res.json();
    applyVersions(vector);
}
```

## 9. API 接口

### 9.1 SSE 事件流

```
GET /api/events
Event: heartbeat
data: {"proxies":790}

Event: heartbeat
data: {"boot":123,"mesh":456,"proxies":790,...}
```

### 9.2 轮询接口

```
GET /api/versions
Response: {"boot":123,"mesh":456,"proxies":790,"rules":101,...}
```

## 10. 优势

✅ **统一管理**：所有版本号集中在一个 bucket  
✅ **持久化**：重启后版本号保持连续  
✅ **简化监听**：只需监听一个 bucket  
✅ **增量高效**：变化时只发送变化的 topic  
✅ **全量兜底**：定期心跳防止错过更新  
✅ **boot 简化**：用版本号标识服务器启动  

## 11. 迁移计划

### 11.1 数据库迁移

```bash
# 创建 versions bucket
# 从现有 VersionNotifier 内存中读取版本号
# 写入 versions bucket
```

### 11.2 代码迁移

1. 实现 `incrementVersion`、`readVersion`、`readAllVersions` 函数
2. 修改 `InitVersionNotifier` 使用 versions bucket
3. 修改 `BumpVersion` 发送增量更新
4. 修改 `Subscribe` 不发送初始数据
5. 更新前端处理逻辑

### 11.3 部署验证

1. 本地编译验证
2. 部署到 QG 环境
3. 验证 SSE 增量通知
4. 验证全量心跳
5. 验证轮询兜底

## 12. 风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| 版本号递增竞态 | 使用 BoltDB 事务保证原子性 |
| SSE 连接断开 | 定期全量心跳 + 轮询兜底 |
| 数据库损坏 | 版本号可从 0 重新开始，不影响功能 |
| 性能开销 | 版本号操作频率低，影响可忽略 |

## 13. 测试计划

- [ ] 单元测试：版本号递增、读取
- [ ] 集成测试：SSE 增量通知
- [ ] 集成测试：全量心跳
- [ ] 集成测试：轮询兜底
- [ ] 部署测试：QG 环境验证
- [ ] 压力测试：高频版本号更新

## 14. 更新日志

- 2026-10-27: 初始设计
