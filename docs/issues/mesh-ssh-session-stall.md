# Mesh SSH 会话僵死问题分析

## 问题描述

通过 mesh 网络建立的 SSH 连接会出现"僵死"现象：
- **表现**：SSH 会话无法交互，无法输入也没有输出，但连接不断开
- **影响**：用户无法正常使用终端，需要手动断开重连
- **复现路径**：本地 → GG 节点（mode B/SOCKS5 入口）→ md.qgw.phn（mesh 域名）

## 连接路径分析

```
┌──────────┐     ┌──────────┐     ┌─────────┐     ┌──────────┐
│ 本地 SSH │────▶│ GG 节点  │────▶│  mesh   │────▶│  QGW     │
│  客户端  │     │ mode B   │     │  网络   │     │ md.qgw   │
└──────────┘     │ SOCKS5   │     └─────────┘     └──────────┘
                 └──────────┘
                      │
                      ▼
                 P2P 连接
                 (可能经中继)
```

### 数据流详解

1. **本地 SSH 客户端** → GG 的 SOCKS5 代理（mode B 入口）
2. **GG 收到连接请求**：目标是 md.qgw.phn
3. **Mesh DNS 解析**：md.qgw.phn → QGW 的 mesh subnet IP（如 100.224.0.1）
4. **Mesh 路由**：GG 查找路由表，找到下一跳 peer
5. **P2P 传输**：通过 P2P 连接发送到 QGW（可能经过中继节点）
6. **QGW 接收**：QGW 的 netstack 收到包，交付给目标服务

## 可能的僵死原因

### 1. P2P 链路质量问题

**现象**：
- P2P 连接 NAT 穿透失败，走 relay 中继
- 中继链路延迟高、丢包率高
- TCP 重传导致 SSH 交互卡顿

**检查方法**：
```bash
# 在 GG 上查看 mesh 状态
curl -sk https://localhost:39998/api/mesh | jq '.peers[] | select(.nodeId=="qgw")'
```

**可能的修复**：
- 优化 P2P 连接策略，优先选择直连路径
- 添加链路质量监控，自动切换到更好的路径

### 2. TCP 缓冲与 Nagle 算法

**现象**：
- SSH 是交互式小数据包
- 如果 TCP_NODELAY 未设置，Nagle 算法会延迟发送（~40ms）
- 累积起来导致明显卡顿

**检查方法**：
- 检查 mode B 代理层是否设置了 TCP_NODELAY
- 检查 mesh P2P 连接是否设置了 TCP_NODELAY

**可能的修复**：
- 确保所有 TCP 连接都设置 TCP_NODELAY
- 参考 `util.SetTCPNoDelay()` 函数

### 3. Mode B 代理层缓冲问题

**现象**：
- SOCKS5 代理有缓冲队列
- 如果下游（mesh）变慢，缓冲区积压
- 导致 SSH 响应延迟

**检查方法**：
- 检查 mode B 实现的缓冲逻辑
- 查看是否有背压（backpressure）机制

**可能的修复**：
- 添加背压机制，当下游慢时及时通知上游
- 减少缓冲队列大小

### 4. Mesh 路由切换丢包

**现象**：
- GG 到 QGW 有多条路径
- 路由切换时可能丢包
- TCP 需要重传，用户感知为卡顿

**检查方法**：
- 查看 GG 的路由表，确认到 QGW 的路径数量
- 监控路由变化频率

**可能的修复**：
- 路由切换时保持旧路径一段时间（make-before-break）
- 添加路径质量评估，避免频繁切换

### 5. Netstack 内部处理延迟

**现象**：
- gVisor netstack 处理包时有延迟
- 特别是在高负载时

**检查方法**：
- 监控 netstack 的包处理延迟
- 检查是否有 goroutine 泄漏

**可能的修复**：
- 优化 netstack 配置
- 增加 worker goroutine 数量

### 6. 连接跟踪表满

**现象**：
- NAT 表或 ModeBTable 满
- 新连接或数据包被丢弃

**检查方法**：
- 检查连接跟踪表的大小和限制
- 查看是否有连接泄漏

**可能的修复**：
- 增加表大小限制
- 优化连接回收机制

## 诊断步骤

### 1. 确认 P2P 连接状态

```bash
# 在 GG 上执行
curl -sk -u admin:changeme https://localhost:39998/api/mesh | jq '.peers[] | select(.nodeId=="qgw") | {nodeId, direct, lastSeen}'
```

- `direct: true` 表示直连
- `direct: false` 表示走中继

### 2. 检查路由路径

```bash
# 在 GG 上执行
curl -sk -u admin:changeme https://localhost:39998/api/mesh | jq '.routes.routes[] | select(.prefix=="100.224.0.0/16")'
```

查看到 QGW 的路由有几条，分别经过哪些节点。

### 3. 监控延迟

```bash
# 在 GG 上持续 ping QGW 的 mesh IP
ping 100.224.0.1
```

观察延迟是否稳定，是否有丢包。

### 4. 检查 TCP 连接状态

```bash
# 在 GG 上查看到 QGW 的 TCP 连接
netstat -an | grep <qgw_ip>:22
```

查看连接状态、重传次数等。

### 5. 查看日志

```bash
# 在 GG 上查看 mesh 相关日志
tail -f /home/layer4/phaethon-gg/phaethon.log | grep -i "mesh\|p2p\|route"
```

## 待确认信息

- [ ] GG 到 QGW 是直接 P2P 还是走中继？
- [ ] 僵死是持续的还是间歇性的？
- [ ] 僵死时 ping/延迟表现如何？
- [ ] 是否有特定的触发条件（如长时间空闲后）？
- [ ] 其他 mesh 节点间是否也有类似问题？

## 修复结论

（待后续补充）

---

**创建时间**：2026-09-27
**状态**：分析中
**优先级**：高（影响日常使用体验）
