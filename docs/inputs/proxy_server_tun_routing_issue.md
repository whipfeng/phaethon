# 代理 Server 流量被 TUN 路由规则错误匹配问题

## 问题描述

所有代理 Server（Direct、Trojan、Socks5、HTTP、Reverse）的流量都可能被 TUN 引擎的路由规则错误匹配，导致连接被重定向到错误的代理。

表现案例：访问 GG 环境的 SSH（`cmll@106.13.183.103 -p 39008`，DirectServer 类型）间歇性失败。

## 排查过程

### 现象
- 端口 39008 是 DirectServer 类型的映射，监听后转发到内网 192.168.1.88:22
- 连接有时成功，有时超时
- 从 GG 控制台日志可以看到连接进入了 DirectServer：
  ```
  [Direct:dyn-map-49c74fd7] TCP 223.104.150.83:26037 → 192.168.1.88:22 → MESH
  ```
- 但随后流量被 TUN 引擎路由到了其他代理：
  ```
  [TUN] TCP 100.179.0.3 → 192.168.1.88:22 → MATCH,Company#TUN@14:00-18:00 → MGMS_HT
  ```

### 根因分析

1. **所有代理 Server 都使用 MeshDial**：`BaseServer.MeshDialWithModeB` 被所有代理类型调用（Direct、Trojan、Socks5、HTTP、Reverse），流量都会经过 TUN 引擎的 netstack
2. **TUN 引擎应用路由规则**：当 SYN 包通过 netstack 时，`acceptTCP` 会查找 modeBTable，然后调用 `handleConn` 进行路由规则匹配
3. **规则匹配问题**：规则如 `MATCH,Company#TUN@14:00-18:00` 本应只匹配来自 TUN mapping 的流量，但代理 Server 的流量也被匹配到了
4. **间歇性原因**：规则有时间范围 `@14:00-18:00`，在这个时间段内流量会被重定向到其他代理，其他时间则正常

### 受影响的代理类型

| Server | inbound 标识 | 文件 |
|--------|--------------|------|
| DirectServer | "Direct" | server/direct.go |
| TrojanServer | "Trojan" | server/trojan.go |
| Socks5Server | "SOCKS5" | server/socks5.go |
| HTTPServer | "HTTP" | server/http.go |
| ReverseServer | "Reverse" | server/reverse.go |

所有类型都调用 `BaseServer.MeshDialWithModeB`，都会经过 TUN 引擎的路由规则匹配。

### 代码流程

```
任意代理 Server.HandleConn
  → BaseServer.MeshDialWithModeB(dstAddr, dstPort, clientAddr, proto, mapping)
    → dialer.MeshDial
      → GlobalNetstackDialWithModeBFunc (即 engine.NetDialWithModeB)
        → 注册 modeBTable
        → ep.Connect (发送 SYN)
          → netstack 路由到 acceptTCP
            → modeBTable.LookupByDst 找到 mapping
            → handleConn(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
              → ruleConf.Match(req, modeBMapping)  ← 在这里可能匹配到错误的规则
```

## 结论

**没有 bug，行为正常。**

日志中看到的 `[TUN] TCP 100.179.0.3 → 192.168.1.88:22 → MATCH,Company#TUN@14:00-18:00 → MGMS_HT` 是流量到达目标节点后的正常路由：

1. DirectServer 接受连接，通过 mesh 网络拨号到目标节点
2. 目标节点从 mesh 收到流量，进入 TUN 接口（源 IP 是 mesh VIP `100.179.0.3`）
3. 目标节点的 TUN forwarder 正确匹配 `#TUN` 规则，路由到 MGMS_HT
4. 这是预期行为，v0.15.1 的 mapping 上下文透传设计工作正常

"间歇性连不上"的原因是规则 `Company#TUN@14:00-18:00` 在特定时间段内将 TUN 流量路由到其他代理，这是规则的预期行为，不是 bug。

## 相关设计（已实现）

**v0.15.0/v0.15.1 Mode B mapping 上下文透传**（`docs/plans/mesh_network_improvements.md` 2.15 节）：

- ModeBTable 直接存 `*config.Mapping` 对象
- forwarder 从 ModeBTable 取出原始 mapping 做规则匹配
- 纯 TUN 流量（Mode A）：`modeBMapping == nil`，用 `TUNMapping`
- Mode B 流量：`modeBMapping != nil`，用原始 mapping

**行为验证**：
- 规则 `MATCH,Company#TUN@14:00-18:00` 只匹配 mapping.Name == "TUN" 的流量
- 代理 Server 的流量（mapping.Name == "dyn-map-XXXXX"）不会被 `#TUN` 规则匹配
- 当流量通过 mesh 到达目标节点后，从目标节点的 TUN 接口进入，此时 mapping 是 TUN，规则正确匹配

## 相关文件

- `server/base.go` - MeshDialWithModeB 实现
- `dialer/bind.go` - MeshDial 实现
- `tun/engine.go` - NetDialWithModeB, acceptTCP, handleConn
- `mesh/modeb.go` - ModeBTable 实现
- `config/config.go` - 路由规则匹配逻辑
- `docs/plans/mesh_network_improvements.md` - v0.15.0/v0.15.1 设计文档
