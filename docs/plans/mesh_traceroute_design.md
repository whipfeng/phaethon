# Mesh Traceroute 设计

## 概述

实现 mesh 网络的 traceroute 支持。每个路由卡口递减 IP TTL，当 TTL=0 时，把包注入 gVisor，由 gVisor 生成 ICMP Time Exceeded 响应。

## 路由卡口

```
┌─────────────────────────────────────────────────────────────────────────┐
│                      路由卡口处理逻辑（统一）                            │
│                                                                         │
│  包到达路由卡口                                                          │
│     │                                                                   │
│     ▼                                                                   │
│  递减TTL                                                                │
│     │                                                                   │
│     ▼                                                                   │
│  TTL=0? ──是──→ 【特殊路由】把TTL=0的包注入本地gVisor                   │
│     │                    │                                              │
│     │                    ▼                                              │
│     │              gVisor收到TTL=0的包                                  │
│     │                    │                                              │
│     │                    ▼                                              │
│     │              gVisor生成ICMP Time Exceeded                         │
│     │                    │                                              │
│     │                    ▼                                              │
│     │              gVisor路由ICMP回源                                   │
│     │                                                                   │
│     否                                                                  │
│     │                                                                   │
│     ▼                                                                   │
│  正常路由（去mesh/进gVisor/转发）                                       │
└─────────────────────────────────────────────────────────────────────────┘
```

## 四个路由卡口

| 卡口 | 位置 | 操作 | 原因 |
|------|------|------|------|
| 卡口1 | 内核路由到TUN | 内核递减 | 内核自动处理 |
| 卡口2 | readLoop | **只检查，不递减** | 内核已经减过了 |
| 卡口3 | HandleMeshFrame(中转) | **递减** | 新的路由跳 |
| 卡口4 | HandleMeshFrame(到达) | **递减** | 最后一跳 |

## 完整场景示例

```
┌─────────────────────────────────────────────────────────────────────────┐
│                      traceroute 100.179.0.1 (GG)                        │
│                         从 QG 发起 (TTL=2)                              │
│                                                                         │
│  QG应用(TTL=2)                                                          │
│     │                                                                   │
│     ▼                                                                   │
│  【卡口1】内核递减 → TTL=1                                              │
│     │                                                                   │
│     ▼                                                                   │
│  readLoop读出(TTL=1)                                                    │
│     │                                                                   │
│     ▼                                                                   │
│  【卡口2】检查TTL                                                       │
│     │        TTL=1 > 0，继续                                            │
│     ▼                                                                   │
│  mesh路由 → 发送到GG                                                    │
│     │                                                                   │
│     ▼                                                                   │
│  GG的HandleMeshFrame收到(TTL=1)                                        │
│     │                                                                   │
│     ▼                                                                   │
│  【卡口4】递减TTL → TTL=0                                               │
│     │                                                                   │
│     ▼                                                                   │
│  TTL=0 → 【特殊路由】                                                   │
│     │                                                                   │
│     ▼                                                                   │
│  把TTL=0的包注入GG的gVisor                                              │
│     │        (src=QG, dst=GG, TTL=0)                                    │
│     ▼                                                                   │
│  GG的gVisor收到TTL=0的包                                                │
│     │                                                                   │
│     ▼                                                                   │
│  gVisor生成ICMP Time Exceeded                                           │
│     │        src=GG的VIP(100.179.0.1)                                   │
│     │        dst=QG的VIP(100.0.0.1)                                     │
│     ▼                                                                   │
│  gVisor路由ICMP → mesh链路 → QG                                         │
│     │                                                                   │
│     ▼                                                                   │
│  QG收到ICMP → 显示第1跳                                                 │
└─────────────────────────────────────────────────────────────────────────┘
```

## 实现要点

1. **readLoop (tun/engine.go)**: 检查 TTL，如果 TTL=0，把包注入 gVisor
2. **HandleMeshFrame (mesh/mesh.go)**: 递减 TTL，如果 TTL=0，把包注入 gVisor
3. **gVisor**: 收到 TTL=0 的包后，自动生成 ICMP Time Exceeded

## 实现状态

### 已完成
- [x] 卡口2 (readLoop): 检查 TTL，如果 TTL=0 注入 gVisor
- [x] 卡口3/4 (HandleMeshFrame): 递减 TTL，如果 TTL=0 注入 gVisor
- [x] 导出 DecrementIPTTL 函数

### 待验证
- [ ] 需要确认 QG 的 TUN 是否正常启动
- [ ] 测试 traceroute 是否能正常工作

### 调试发现
- QG 配置显示 `tun: enabled: true`，但日志中没有 TUN 启动信息
- 进程有 /dev/net/tun 打开，但没有 "TUN device started" 日志
- 需要进一步调查 TUN 未启动的原因

## 测试计划

### 本地测试（推荐）

本地 macOS 通过 QG 的旁路网关 (bypass-gateway) 连接到 mesh 网络，可以直接测试 traceroute：

```bash
# 本地测试到 GG (100.179.0.1)
traceroute -n -w 2 -m 5 100.179.0.1

# 本地测试到 JF (100.2.0.1)
traceroute -n -w 2 -m 5 100.2.0.1
```

**网络路径**：
```
本地 macOS → QG 旁路网关 (192.168.1.101) → QG TUN → mesh 网络 → 目标节点
```

**前提条件**：
- QG 的 TUN 和 bypass-gateway 已启用
- 本地 DNS 配置为 QG (192.168.1.101) 或使用静态路由
- 目标 mesh VIP 已知（如 100.179.0.1 = GG）

### 远程测试

在 GG 环境执行（需要安装 traceroute）：
```bash
ssh layer4@106.13.183.103
traceroute -n 100.0.0.1  # 到 QG
```

### 预期结果

- 每一跳显示 mesh 节点的 VIP
- 中间节点返回 ICMP Time Exceeded
- 最终到达目标节点
