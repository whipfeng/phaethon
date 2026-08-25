# tun_spec.md

## 元数据

- 文档类型：Spec
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-07-14

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-07-14 | 初始版本：TUN 模式规格 | Claude |

## 1. 概述

TUN 模式通过系统 TUN 接口 + gvisor 用户态网络栈 + Fake-IP 实现系统级流量拦截，将系统全部流量路由到 phaethon 处理。

## 2. 架构

```
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  系统应用    │────→│  TUN 接口    │────→│ gvisor 栈   │
└─────────────┘     └──────────────┘     └──────┬──────┘
                                                │
                       ┌────────────────────────┘
                       ↓
              ┌─────────────────┐
              │ DNS Hijacker    │  53 端口 DNS 拦截
              │ TCP Forwarder   │  连接 → RuleConf.Match → ChainDial
              │ UDP Forwarder   │  UDP 包 → NAT → ChainDial
              └─────────────────┘
```

## 3. 核心组件

### 3.1 TUN 接口

- 创建系统 TUN 设备，需要管理员/root 权限。
- 配置路由和 DNS，使系统流量进入 TUN。
- Windows 依赖 `wintun.dll`。

### 3.2 gvisor 网络栈

- 在 `tun/engine.go` 中初始化 netstack。
- 设置 TCP 传输处理器。
- UDP 传输处理器（待完善或已实现，视版本而定）。

### 3.3 Fake-IP

- DNS 查询返回 Fake-IP（私有地址段）。
- 应用连接 Fake-IP 时，gvisor 栈根据 Fake-IP 映射回真实域名/地址。
- 避免 DNS 泄漏，支持域名规则匹配。

### 3.4 DNS Hijacker

- 拦截 53 端口 DNS 请求。
- 根据规则返回 Fake-IP 或转发到真实 DNS。

## 4. 配置

```yaml
tun:
  enabled: true
  name: "phaethon-tun"
  mtu: 1500
  address: 198.18.0.1/16
  dns-hijack: true
```

## 5. Admin API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/tun` | 获取 TUN 状态（启用/禁用、接口名、错误信息） |
| POST | `/api/tun` | 启用或禁用 TUN |

请求体示例：

```json
{ "enabled": true }
```

## 6. 限制与注意事项

- 需要管理员/root 权限。
- Windows 需要 `wintun.dll`。
- TUN 启用后会修改系统路由/DNS，请谨慎操作。
- 当前 UDP 转发能力取决于实现版本。

## 7. 相关链接

- [admin_spec.md](admin_spec.md)
- [protocol_spec.md](protocol_spec.md)
