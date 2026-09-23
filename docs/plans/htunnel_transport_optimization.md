# h_tunnel 传输优化：共享 Client + RTT 节奏合帧批量

| | |
|---|---|
| 版本 | v0.1.0 |
| 状态 | DRAFT |
| 日期 | 2026-09-17 |
| 关联 | `docs/plans/p2p_control_frame_congestion.md`（P2P 层写路径去 deadline，本文处理链路层吞吐） |

## 版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| v0.1.0 | 2026-09-17 | 初稿：共享 http.Client + 无定时器合帧批量（写出期间同步收集） |

## 一、背景与问题

### 1.1 现状

`dialer/htunnel.go` 每个连接的行为：

- **每拨号新建 Client**：`dialHTunnel()`（:121）和 `DialPacket()`（:457）各自调用 `NewHTunnelHTTPClient(proxy)` 新建一个 `http.Client` + `http.Transport`。跨连接完全不复用 TCP/TLS，旧 Transport 的空闲连接只能等 GC 回收（轻微泄漏）。
- **每次 Write 一次同步 POST**：`htunnelConn.Write()`（:303）持 `writeMu`，组装一个 POST，阻塞 10s ctx 等响应，完成后才返回。吞吐上界 = 单次 Write 大小 / RTT。上层 `io.Copy`（32KB 块）驱动时 ≈ 32KB/RTT（150ms RTT ≈ 213KB/s）。
- **Transport 零调优**：未设 `MaxIdleConnsPerHost`（默认 2）、`IdleConnTimeout` 等，连接复用全靠默认行为。

### 1.2 问题

1. 吞吐受 RTT 惩罚：每 32KB 付一次完整 HTTP 请求开销（请求行/头、nginx 转发、服务端 seq 状态机往返）。
2. 连接建立贵：每条 htunnelConn 独立 TCP（+TLS）握手；h_tunnel 映射高并发时握手风暴。
3. VM→GG→MS10 两跳中继实测 7.9MB 推送因链路停顿超时失败——慢链路上每字节都付 RTT 税，雪上加霜。

### 1.3 目标 / 非目标

**目标**：
- 同一 proxy 的所有 h_tunnel 连接共享底层连接池（TCP/TLS 复用）。
- 连续小写入自动合并为大批 POST，吞吐从 32KB/RTT 提升一个数量级。
- **零人工延迟**：不用定时器攒批；无积压时单次写延迟与现状完全一致。

**非目标**：
- 不改 wire 协议（URL 格式、X-* 头、seq 纪律、加密方式全部不变）。
- 不改读路径（长轮询 GET）与心跳（PUT 30s）。
- 不改 UDP PacketConn（数据报小且离散，批量无收益，保持同步写）。

## 二、关键前提：为什么 Write 必须在 in-flight 期间立即返回

htunnelConn 的上层写入者**都是单 goroutine**：

- P2P peer 连接：`peerWriteLoop` 是唯一 `conn.Write` 调用者（p2p/p2p.go:136）。
- 代理隧道流量：每个方向一个 `io.Copy` goroutine。

如果 Write 保持现状的同步阻塞（调用者即发送者），POST 期间写入者自己被卡死，**永远不存在"POST 在飞时又来了新数据"**——收集机制无从触发，批量等于没做。

因此本设计把 Write 改为**异步缓冲语义**：

> 有数据立即写出（首次写触发发送者立即 POST，不等任何定时器）；POST 在飞期间新到的 Write 立即返回（数据进 pending 收集）——反正这个时候也写不出去；POST 完成后发送者立即把收到的数据发下一批，循环直到 pending 清空。

批量节奏完全由 RTT 自然形成：链路快 → POST 很快回来 → 批小；链路慢 → POST 在飞期间积累多 → 批大。这正是 TCP Nagle 想做而 HTTP 隧道层缺失的事。

## 三、设计

### 3.1 共享 http.Client（按 proxy 缓存）

```go
// dialer/htunnel.go

type htClientEntry struct {
    client *http.Client
    sig    htClientSig // 构建时的配置快照，用于失效判断
}

type htClientSig struct {
    server string
    port   int
    url    string
    next   *config.Proxy // 指针身份，via 链变化即失效
}

var htunnelClients sync.Map // proxy.Name -> *htClientEntry

func sharedHTunnelClient(proxy *config.Proxy) *http.Client {
    sig := htClientSig{server: proxy.Server, port: proxy.Port, url: proxy.URL, next: proxy.Next}
    if v, ok := htunnelClients.Load(proxy.Name); ok {
        e := v.(*htClientEntry)
        if e.sig == sig {
            return e.client
        }
        // 配置变了（reload 改了 via 链等）：旧 client 留给在飞请求自然耗尽，新请求用新 client
        e2 := &htClientEntry{client: newHTunnelTransport(proxy), sig: sig}
        htunnelClients.Store(proxy.Name, e2)
        return e2.client
    }
    e := &htClientEntry{client: newHTunnelTransport(proxy), sig: sig}
    actual, _ := htunnelClients.LoadOrStore(proxy.Name, e)
    return actual.(*htClientEntry).client
}

func newHTunnelTransport(proxy *config.Proxy) *http.Client {
    c := NewHTunnelHTTPClient(proxy) // 现有函数，DialContext via 链逻辑不变
    t := c.Transport.(*http.Transport)
    t.MaxIdleConns = 64
    t.MaxIdleConnsPerHost = 16
    t.IdleConnTimeout = 120 * time.Second
    return c
}
```

要点：

- **缓存键 = `proxy.Name`**。`dialHTunnel` / `DialPacket` 中的 `client := NewHTunnelHTTPClient(proxy)` 全部替换为 `client := sharedHTunnelClient(proxy)`。
- **失效条件**：`server/port/url` 变化或 `Next` 指针变化（配置 reload 换了 via 链）。换出的旧 client 不主动 Close（`http.Transport` 无强制关闭 API），在飞请求结束后其空闲连接被 GC；新连接走新 client。
- **池参数**：每条 conn 峰值并发 ≈ 3（1 长轮询 GET + 1 POST + 心跳 PUT 瞬时），默认 `MaxIdleConnsPerHost=2` 不够多条 conn 复用，提到 16。`IdleConnTimeout=120s` 大于心跳间隔 30s，避免池内连接被提前掐掉导致反复握手。
- **收益**：同一 proxy 的 N 条 conn 共享握手；`MaxIdleConnsPerHost` 内的 POST/心跳直接复用已建立连接。

### 3.2 合帧批量：sender goroutine + RTT 节奏收集

#### 状态与角色

```go
type htunnelConn struct {
    // ... 现有字段不变 ...

    // 写批量状态（pendMu 保护）
    pendMu     sync.Mutex
    pending    []byte      // 已接收待发送的数据
    sending    bool        // sender goroutine 存活标志
    sendCond   *sync.Cond  // sender 通知 + 背压唤醒
    writeErr   error       // 最近一次 POST 失败（异步错误传播）
    writeSeq   int
}
```

`writeMu` 取消（角色移交 sender goroutine），`seqMu` 并入 `pendMu`。

#### Write：立即触发，in-flight 期间只收集

```go
const (
    htMaxBatchBytes     = 512 << 10 // 单个 POST body 上限（nginx 默认 client_max_body_size=1m 的安全余量）
    htPendingHighWater  = 1 << 20   // pending 积压上限，超过则 Write 阻塞（背压）
)

func (c *htunnelConn) Write(b []byte) (int, error) {
    c.pendMu.Lock()
    defer c.pendMu.Unlock()

    // 已有 POST 失败：立即上抛，让上层（io.Copy / peerWriteLoop）拆连接
    if c.writeErr != nil {
        err := c.writeErr
        c.writeErr = nil
        return 0, err
    }
    select {
    case <-c.closed:
        return 0, io.ErrClosedPipe
    default:
    }

    // 背压：积压超过高水位，等 sender 消化（连接关闭时唤醒报错）
    for len(c.pending) >= htPendingHighWater && c.writeErr == nil {
        c.sendCond.Wait()
        if c.writeErr != nil { /* 同上，返回错误 */ }
        select { case <-c.closed: return 0, io.ErrClosedPipe; default: }
    }

    c.pending = append(c.pending, b...)

    if !c.sending {
        c.sending = true
        go c.sendLoop() // 立即写出，无定时器
    }
    return len(b), nil
}
```

#### sendLoop：一有空就发，POST 完成即取下一批

```go
func (c *htunnelConn) sendLoop() {
    for {
        c.pendMu.Lock()
        if c.writeErr != nil || len(c.pending) == 0 {
            c.sending = false
            c.sendCond.Broadcast() // 唤醒背压中的 Write
            c.pendMu.Unlock()
            return
        }
        n := len(c.pending)
        if n > htMaxBatchBytes {
            n = htMaxBatchBytes
        }
        batch := make([]byte, n)
        copy(batch, c.pending)
        if n == len(c.pending) {
            c.pending = c.pending[:0]
        } else {
            c.pending = c.pending[n:]
        }
        c.pendMu.Unlock()

        err := c.postBatch(batch) // 现有 POST 逻辑：seq++、SealBody、POST、状态码检查

        c.pendMu.Lock()
        if err != nil {
            c.writeErr = err
            c.pending = c.pending[:0] // 连接已死，丢弃积压
            c.sendCond.Broadcast()
        }
        c.pendMu.Unlock()
        if err != nil {
            return
        }
    }
}
```

#### 时序示例（io.Copy 32KB 块驱动，RTT=150ms）

```
t=0      Write(32KB#1) → pending=[#1] → 启动 sendLoop → POST(#1) 在飞
t=0..150 Write(#2..#5) 立即返回，pending=[#2#3#4#5]（约 128KB，此刻反正写不出去）
t=150    POST(#1) 返回 → 立即 POST(#2..#5 合并批) 在飞
t=150..  Write(#6..) 继续收集
t=300    POST(合并批) 返回 → ...
```

吞吐从 32KB/RTT 变为 min(到达速率, 512KB/RTT)，150ms RTT 下上界 ≈ 3.4MB/s（对比现状 213KB/s，16 倍）。

#### 语义变化与代价（明确列出）

| 项 | 现状 | 新设计 |
|------|------|--------|
| Write 返回时机 | POST 完成后（同步） | 数据入 pending 即返回；POST 在飞时不阻塞（高水位除外） |
| POST 失败错误 | 当场返回给调用者 | **延迟**：存 `writeErr`，下次 Write 上抛；读侧（长轮询/心跳）几乎同时也会报错 |
| 数据落盘保证 | 返回即 POST 完 | 返回 ≠ 已发出（缓冲语义，同 TCP） |
| Close 时未发数据 | 不存在（Write 同步） | **丢弃**（上层应写完再 Close，与现状等价——现状 Write 不返回 Close 也不会发生） |

错误传播链验证：

- **代理隧道流量**：`io.Copy` 写方向靠 Write 错误退出；读方向 htunnelConn.Read 的长轮询在服务端连接死后返回 410/错误 → 两个方向都会断，符合现状行为。
- **P2P peer 连接**：`peerWriteLoop` 当前靠 `Write` 返回错误杀连接。异步化后错误延迟到下一帧 Write（心跳 10s 一帧，检测延迟上界 10s）。**这与 `p2p_control_frame_congestion.md` v0.3.0 的结论一致**：连接死亡检测本就以读侧 60s deadline 为准，写侧错误从来不是主要检测手段。

#### seq 纪律（不变量的保持）

服务端要求每个方向的 seq **严格 +1 递增、各用一次**（`step != 1` → 410，server/htunnel.go:773）。新设计中 seq 分配只发生在 `postBatch`（sender goroutine 串行执行），天然严格串行，纪律不变。心跳 PUT（connSeq 计数器）与 DELETE 与写 seq 属不同命名空间，互不影响，保持现状。

### 3.3 不变部分

| 部分 | 处理 |
|------|------|
| 读路径（长轮询 GET，40s/408 重试） | 不动 |
| 心跳（PUT，10s 首跳 + 30s 周期） | 不动（自动受益于共享 client） |
| 三步握手（HEAD×3） | 不动（自动受益） |
| 加密（XChaCha20-Poly1305 SealBody/OpenBody） | 不动（批量在明文层合并，加密按批整体进行，现状即整 body 加密） |
| UDP `htunnelPacketConn` | WriteTo 保持同步，不改 |
| URL 格式 / X-* 头 | 不动 |

## 四、兼容性

### 4.1 新旧混布

- 客户端改动对服务端**完全透明**：URL、头、seq 纪律均不变，服务端 `io.ReadAll(r.Body)` 无大小限制（server/htunnel.go:780）。新客户端直接对接现有服务端，**无需同步升级**。
- 唯一约束：POST body ≤ 中间设备限制。JF 链路经 nginx（`/ui/resdata/` → 10.140.51.2:32457），**nginx 默认 `client_max_body_size 1m`**，`htMaxBatchBytes=512KB` 已留安全余量。若未来调大批量需同步检查 nginx 配置。

### 4.2 与 P2P 写路径改造的关系

正交且互补：`p2p_control_frame_congestion.md` 解决"P2P 帧在内核 socket 缓冲的排队与写 deadline 误杀"；本文解决"htunnel 链路层每 RTT 只能推 32KB"。两跳中继（VM→GG→MS10）的 7.9MB 推送失败是两者叠加的结果，需都落地后复测。

## 五、文件变更清单

| 文件 | 变更 |
|------|------|
| `dialer/htunnel.go` | 新增 `sharedHTunnelClient`（sync.Map 缓存 + sig 失效 + 池参数）；`dialHTunnel`/`DialPacket` 改用共享 client；`htunnelConn` 增加批量字段（pendMu/pending/sending/sendCond/writeErr），删除 writeMu/seqMu；重写 `Write`（立即返回 + 收集 + 背压），新增 `sendLoop`/`postBatch`；`Close` 唤醒 sendLoop 与背压中的 Write |

## 六、参数汇总

| 参数 | 值 | 依据 |
|------|------|------|
| `htMaxBatchBytes` | 512KB | nginx 默认 `client_max_body_size=1m` 的安全余量；512KB/RTT 已远超链路能力 |
| `htPendingHighWater` | 1MB | ≈ 2 个满批；恢复类 TCP 背压，限制最坏内存；链路 200KB/s 时对应约 5s 缓冲 |
| `MaxIdleConnsPerHost` | 16 | 每条 conn 峰值 3 并发（GET/POST/PUT），多条 conn 复用 |
| `MaxIdleConns` | 64 | 进程级上限 |
| `IdleConnTimeout` | 120s | > 心跳间隔 30s，避免池内连接被掐 |

## 七、验证方案

1. **单连接基础功能**：h_tunnel 映射代理流量正常（HTTP/SOCKS 走 h_tunnel 出口），对比升级前后无行为差异。
2. **吞吐对比**：同一 h_tunnel 链路（建议 MGMS_HT→JF）`curl` 拉取大文件，对比升级前后速率；观察 `[HTUNNEL-CLI]` 日志确认 POST 频率下降、单批变大。
3. **零延迟确认**：SSH 交互流量（小包、离散写）延迟无劣化——孤立小写应触发立即 POST，不出现攒批等待。
4. **连接复用确认**：多条 conn 并发时抓包/日志确认 TCP 建立数远小于 conn 数；旧版每个 POST 新建 TCP 的现象消失。
5. **错误传播**：拔掉 h_tunnel 服务端网络 → 确认 Write 方向在下一帧报错、读方向长轮询报错、连接被拆、心跳 410 日志出现。
6. **背压**：构造慢链路 + 大流量（复现 VM→GG→MS10 场景）→ 确认 pending 有界（内存平稳）、无 OOM、流完成后数据完整（md5 对比）。
7. **P2P over h_tunnel**：QG↔JF 的 P2P 链路经 h_tunnel，确认 hello/gossip/mesh 数据正常、心跳保活正常。
