# Multi-NIC 架构问题分析

## 当前数据流（DNS 查询示例）

```
应用 (172.30.0.2)
  │
  │ DNS 查询: src=172.30.0.2, dst=100.1.0.3 (GIP)
  ▼
TUN 设备
  │
  │ readLoop 读取
  ▼
NIC 1 (TUN, 无地址绑定, 非混杂)
  │
  │ gVisor 路由检查: dst=100.1.0.3
  │ 100.1.0.3 绑定到 NIC 2 → 转发到 NIC 2
  ▼
NIC 2 (Mesh, 绑定 GIP=100.1.0.3)
  │
  │ 本地交付 → DNS Forwarder
  ▼
DNS Forwarder 处理查询，生成响应
  │
  │ 响应: src=100.1.0.3, dst=172.30.0.2
  ▼
gVisor 路由检查: dst=172.30.0.2
  │
  ├─ 匹配 VIP (.1/32)? ❌ 否
  ├─ 匹配 hostIP (.2/32)? ❌ 否
  ├─ 匹配 mesh (100.0.0.0/8)? ❌ 否
  └─ 匹配 default (0.0.0.0/0)? ✅ 是 → NIC 3
  │
  ▼
NIC 3 (Loopback, 混杂模式)
  │
  │ WritePackets: 环回到入站
  ▼
gVisor 再次检查: dst=172.30.0.2
  │
  │ 172.30.0.2 不是本地地址（未绑定到任何 NIC）
  │ 即使 NIC 3 是混杂模式，也不是"本地交付"
  │
  └─→ ❌ 包被丢弃或循环，writePackets=0
```

## 问题根因

**gVisor 内部生成的响应包（DNS、Proxy）无法返回给 TUN**

- 响应包的目标是应用 IP（如 172.30.0.2）
- 应用 IP 不在任何路由中（除了 default → NIC 3）
- NIC 3 环回后，目标仍不是本地地址
- 包无法到达 NIC 1，无法写回 TUN

## 当前路由表

```go
{Destination: 100.1.0.1/32, NIC: 1},  // VIP
{Destination: 100.1.0.2/32, NIC: 1},  // hostIP
{Destination: 100.0.0.0/8, NIC: 2},   // mesh network
{Destination: 0.0.0.0/0, NIC: 3},     // default → loopback
```

## 缺失的路由

**需要将应用网段路由到 NIC 1**，让响应包能返回 TUN：

```go
{Destination: 172.30.0.0/24, NIC: 1}, // 应用网段 → NIC 1 (缺失!)
```

或者：

**启用 NIC 1 的混杂模式**，让它接收所有返回的包。

## 设计文档的缺陷

设计文档只考虑了：
- ✅ 出站流量（TUN → gVisor → mesh/external）
- ✅ 回程流量（mesh → gVisor → TUN，目标 VIP）

没有考虑：
- ❌ gVisor 内部生成的响应（DNS、Proxy）如何返回 TUN
- ❌ 响应包的目标是应用 IP，不是 VIP
