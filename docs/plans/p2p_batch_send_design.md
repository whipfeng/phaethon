# P2P 层统一攒批发送设计

## 背景

当前 h_tunnel v1 的 POST 方向（客户端→服务端）没有攒批，每个 IP 包触发一次 HTTP POST，效率低：
- 小包（TCP ACK 40-52 字节）→ 500 字节 HTTP 头 + 加密开销
- 高并发时大量 HTTP 请求，服务端处理压力大

历史上 h_tunnel 层曾实现过攒批（`pending` 缓冲 + `flushLoop`），但在 `f7fc750` 中被移除，改为 fire-and-forget。

## 问题

链路层（h_tunnel / direct P2P）各自实现攒批逻辑重复且不一致。P2P 层已有 `writeCh` 排队帧，但 `peerWriteLoop` 每次只取一帧调用 `transport.Send()`，无法自然攒批。

## 设计目标

1. **统一攒批点**：P2P 层的 `peerWriteLoop` 负责攒批，链路层不再关心
2. **链路层简化**：`FrameTransport` 接口支持批次发送，实现只管"给什么发什么"
3. **控制帧优先级**：控制帧（hello/gossip/ACK）仍单独发送，不攒批，保证低延迟
4. **向后兼容**：单帧发送仍支持，批次是可选优化

## 架构

```
P2P 层 (peerWriteLoop)
  │
  ├─ controlCh → 控制帧 → transport.Send(frame) 单独发
  │
  └─ writeCh → 数据帧 → coalesce → transport.SendBatch(frames) 批量发
```

### peerWriteLoop 改动

```go
for {
    // 1. 控制帧优先（非阻塞）
    select {
    case req := <-peer.controlCh:
        writeFrame(req, true)  // 单独发
        continue
    default:
    }
    
    // 2. 阻塞等第一帧
    select {
    case <-peer.stopCh:
        return
    case req := <-peer.controlCh:
        writeFrame(req, true)
        continue
    case req := <-peer.writeCh:
        // 3. 非阻塞攒更多数据帧
        batch := []writeReq{req}
        for len(batch) < maxBatchFrames {
            select {
            case more := <-peer.writeCh:
                batch = append(batch, more)
            default:
                goto sendBatch
            }
        }
    sendBatch:
        // 4. 批量发送
        writeBatch(batch)
    }
}
```

### FrameTransport 接口扩展

```go
type FrameTransport interface {
    // Send 发送单帧（控制帧或兼容旧实现）
    Send(frameType byte, payload []byte, isControl bool) error
    
    // SendBatch 发送多帧批次（数据帧优化路径）
    // 默认实现：逐个调用 Send
    SendBatch(frames []Frame) error
    
    Recv() (frameType byte, payload []byte, err error)
    Close() error
}

type Frame struct {
    Type    byte
    Payload []byte
}
```

### 链路层实现

**设计原则**：链路层自己分批循环，按自己的承载能力发送。P2P 层交给链路层的批次可能很大，链路层根据自己的限制（如 HTTP body 大小、TCP 缓冲区）拆分成多次发送。

#### streamTransport（direct P2P）

```go
func (t *streamTransport) SendBatch(frames []Frame) error {
    t.writeMu.Lock()
    defer t.writeMu.Unlock()
    
    var buf bytes.Buffer
    for _, f := range frames {
        frame.WriteFrame(&buf, f.Type, f.Payload)
    }
    _, err := t.conn.Write(buf.Bytes())  // 一次系统调用，TCP 层自己分段
    return err
}
```

无应用层限制，TCP 层会自动处理大数据的分段。

#### htunnelDirectTransport（h_tunnel v1）

```go
func (t *htunnelDirectTransport) SendBatch(frames []Frame) error {
    // 按 512KB 限制分批 POST（与 nginx client_max_body_size 对齐）
    const maxPostSize = 512 * 1024
    
    var buf bytes.Buffer
    for _, f := range frames {
        frameBytes := serializeFrame(f.Type, f.Payload)
        
        // 超过限制就发一批
        if buf.Len()+len(frameBytes) > maxPostSize && buf.Len() > 0 {
            if err := t.postBatchAndReset(&buf); err != nil {
                return err
            }
        }
        buf.Write(frameBytes)
    }
    
    // 发最后一批
    if buf.Len() > 0 {
        return t.postBatchAndReset(&buf)
    }
    return nil
}
```

链路层内部循环，按自己的承载能力（512KB/POST）分批发送，对 P2P 层透明。

#### meshChannelTransport（服务端）

```go
func (t meshChannelTransport) SendBatch(frames []Frame) error {
    for _, f := range frames {
        select {
        case t.ch.meshOut <- meshMsg{frameType: f.Type, payload: f.Payload}:
        case <-t.ch.closed:
            return io.ErrClosedPipe
        }
    }
    return nil
}
```

逐帧入队，GET 长轮询侧已有 coalesce 逻辑（`meshHandleRead` 中 `for buf.Len() < maxBatchSize`）。

## 参数

- `maxBatchFrames`：单次批次最大帧数，默认 64
- 批次大小上限：512KB（与 h_tunnel nginx 限制对齐）
- 无定时器：非阻塞 coalesce，有就取，没有就发

## 收益

1. **h_tunnel POST 方向**：多帧打包成一次 HTTP POST，减少请求数和开销
2. **direct P2P**：多帧一次系统调用写入，减少 TCP 分段
3. **统一逻辑**：所有链路层自动受益，无需各自实现攒批
4. **控制帧不受影响**：仍单独发送，保证低延迟

## 测试

1. 单元测试：验证 SendBatch 序列化正确
2. 集成测试：QG→JF P2P 传输，检查日志确认批次行为
3. 性能对比：小包场景下的吞吐量和延迟

## 部署

1. 编译诊断版本，部署 QG 和 JF
2. 开启 `mesh.forwarding-trace`，观察 `p2p_write_start` 事件中的批次大小
3. 验证功能正常后关闭 trace
