# Mesh 稳定性改进

## 背景

1. SSH 通过 GG 的 mode B 入口连接到 md.qgw.phn，终端经常假死
2. Mesh 收敛后路径选择不考虑丢包率
3. eventCh 容量不足可能丢弃 topology 事件
4. reconnect backoff 无 jitter，多节点同时重启时同步重试

## 修改范围

### 1. TCP Keepalive（修复 SSH 假死）

**问题**：
- inbound TCP endpoint 设了 keepalive 参数但没调 `SetKeepAlive(true)`
- outbound dial 路径完全没设 keepalive
- 链路断了之后 TCP 等重传超时（可能 60s+），用户看到终端卡住

**修复**：
- `config/config.go`：MeshConfig 加 `TCPKeepalive` 字段
  ```go
  type MeshTCPKeepalive struct {
      Idle     int `yaml:"idle" json:"idle"`         // 空闲时间（秒），默认 30
      Interval int `yaml:"interval" json:"interval"` // 探测间隔（秒），默认 10
      Count    int `yaml:"count" json:"count"`       // 探测次数，默认 3
  }
  type MeshConfig struct {
      // ... existing fields ...
      TCPKeepalive *MeshTCPKeepalive `yaml:"tcp-keepalive,omitempty" json:"tcp-keepalive,omitempty"`
  }
  ```
- `mesh/netstack.go`：从 config 读取 keepalive 参数，应用到所有 TCP endpoint
  - inbound：加 `ep.SetSockOpt(&tcpip.KeepaliveEnabledOption{Enabled: true})`
  - outbound dial（NetDial, NetDialWithPreConnect, NetDialWithModeB）：加 keepalive 选项
- Admin API：已有 `PUT /api/config/mesh`，自动支持
- Admin UI：mesh 配置页加 TCP keepalive 配置项

### 2. eventCh 扩大

**问题**：eventCh 容量 64，topology 变化快时可能丢弃 Register 事件

**修复**：
- `p2p/p2p.go`：eventCh 容量 64 → 1024
- drop 时打 LogWarning 日志（当前是 LogDebug）

### 3. Reconnect Backoff 加 Jitter

**问题**：backoff 确定性（1s→2s→4s→...→60s），多节点同时重启时同步重试

**修复**：
- `p2p/p2p.go`：backoff 计算加随机偏移
- 公式：`delay = baseDelay * (0.5 + rand.Float64())`（0.5~1.5 倍）
- cap 保持 60s

### 4. P2P 心跳超时强拆（修复拓扑残留）

**问题**：
- P2P 会话只在 `transport.Recv()` 返回错误时才结束
- 如果对端不响应但 TCP 连接还在（如对端进程卡死、mesh 未启动），会话永远不会结束
- `UnregisterPeer` 不会被调用，导致拓扑残留：
  - 拓扑邻居表还保留该节点
  - IP 路由还经过该节点
  - 域名路由还指向该节点
  - 其他节点继续向该节点发送 gossip，但得不到响应
- 用户看到的现象：节点"断而不死"，路由选择了一条实际上不通的链路

**修复**：
- `p2p/p2p.go`：在 `runSession` 中添加心跳超时检测
- 超时阈值：30 秒无心跳（3 次心跳周期，心跳间隔 10 秒）
- 超时后主动关闭连接，触发 defer 中的 `UnregisterPeer`
- 实现：
  ```go
  func (m *P2PManager) runSession(peer *Peer) {
      heartbeatTimeout := 30 * time.Second
      checkInterval := 10 * time.Second
      ticker := time.NewTicker(checkInterval)
      defer ticker.Stop()
      
      go func() {
          for {
              select {
              case <-peer.stopCh:
                  return
              case <-ticker.C:
                  if time.Since(peer.LastSeen) > heartbeatTimeout {
                      util.LogWarn("[P2P] heartbeat timeout for %s, closing connection", peer.ID)
                      peer.transport.Close()
                      return
                  }
              }
          }
      }()
      
      // ... existing runSession logic ...
  }
  ```

**清理链路**：
- `transport.Close()` → `Recv()` 返回错误 → `runSession` 退出
- defer 触发 → `UnregisterPeer(sender)` 被调用
- `UnregisterPeer` 从拓扑表移除该 peer
- 下次路由计算时，该 peer 的路由自动失效
- 下次 gossip 时，该 peer 的域名路由自动清除

### 5. selectBestPeer 综合评分

**问题**：
- 当前只看 RTT，不看丢包率
- probe 发出去没回 → RTT 窗口保留旧值 → 继续选已断链路
- 无探测数据时 fallback 到 hash（确定性，不分散流量）

**修复**：
- `mesh/mesh.go` selectBestPeer：用 effectiveRTT = avgRTT / (1 - packetLoss) 选最优
- 丢包 50% → effectiveRTT 翻倍，丢包 90% → 10 倍
- 无探测数据时 fallback 到随机选择（`rand.Intn(len(candidates))`）

## 影响范围

- `config/config.go`：MeshConfig 加 TCPKeepalive 字段
- `mesh/netstack.go`：TCP endpoint keepalive
- `mesh/mesh.go`：selectBestPeer 评分逻辑
- `p2p/p2p.go`：eventCh 容量、backoff jitter、心跳超时检测
- `admin/static/app.js`：mesh 配置页加 TCP keepalive 配置项

不影响：
- 路由表计算逻辑
- IPIP 封装逻辑
- sticky cache（nodeID 选择）
- 现有 P2P 连接行为（keepalive 是额外保护，不改变正常路径）
- Admin API 结构（已有 PUT /api/config/mesh）

## 验证

1. 构建成功
2. 部署到 QG 环境
3. SSH 通过 GG mode B 连接到 md.qgw.phn，验证不再假死
4. Admin UI 验证 TCP keepalive 配置可读写
5. 检查日志确认 eventCh drop 告警、jitter 生效、selectBestPeer 评分逻辑
