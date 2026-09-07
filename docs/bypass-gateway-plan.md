# QG 旁路网关方案

## 状态：DNS 架构已验证，待部署旁路网关

目录：`/root/bypass-gateway/`

## 概述

QG 环境 (10.0.0.40 / 192.168.0.100) 作为旁路网关，为局域网设备提供透明代理。

## 网络拓扑

```
[主路由 192.168.1.1]
       |
       | LAN
       |
[QG 旁路网关 192.168.0.100]
  - eth1: 192.168.0.100 (旁路网关接口)
  - eth0: 10.0.2.15 (管理口，SSH 访问)
  - Phaethon TUN 模式
       |
[局域网设备]
  网关 → 192.168.0.100
```

## 目录结构

```
/root/bypass-gateway/
├── config/
│   └── config.yaml     # 旁路专用配置 (admin:39998, TUN 模式, DNS:53)
├── scripts/
│   ├── start.sh        # 启动旁路网关
│   ├── stop.sh         # 停止旁路网关
│   └── iptables.sh     # iptables 规则（仅 NAT）
└── logs/
    └── phaethon.log    # 运行日志
```

## 快速使用

```bash
# 启动
/root/bypass-gateway/scripts/start.sh

# 停止
/root/bypass-gateway/scripts/stop.sh

# 查看日志
tail -f /root/bypass-gateway/logs/phaethon.log
```

## 端口分配

| 服务 | 主 Phaethon | 旁路网关 |
|------|-------------|----------|
| Admin | 39999 | 39998 |
| DNS | - | 53 |

## 设计原理

### 核心思路

旁路网关 = Windows TUN 模式 + IP 转发 + 源 IP 替换

```
┌─────────────────────────────────────────────────────────────────┐
│                    旁路网关 (192.168.0.100)                       │
│                                                                 │
│  客户端流量 (网关=192.168.0.100)                                 │
│       │                                                         │
│       ▼                                                         │
│  eth1 收到包                                                     │
│       │                                                         │
│       ▼                                                         │
│  路由决策: 默认路由 → tun0                                        │
│       │                                                         │
│       ▼                                                         │
│  TUN 虚拟网卡 → Phaethon 处理                                    │
│       │                                                         │
│       ├─ PROXY: 拨号代理服务器（新连接，源IP=旁路网关）            │
│       │                                                         │
│       └─ DIRECT: 直接发出（源IP=旁路网关）                        │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 与 Windows TUN 模式对比

| 功能 | Windows TUN | 旁路网关 TUN |
|------|-------------|--------------|
| 流量来源 | 本机进程 | 其他设备转发 |
| 默认路由 → TUN | ✓ | ✓ |
| IP 转发 | 不需要 | `ip_forward=1` |
| 源 IP 替换 | 不需要 | MASQUERADE 或内化 |

### 需要的额外配置（相比 Windows TUN）

```bash
# 1. 开启 IP 转发
sysctl -w net.ipv4.ip_forward=1

# 2. 源 IP 替换（让回程包回到旁路网关）
iptables -t nat -A POSTROUTING -o eth1 -j MASQUERADE
```

### 源 IP 替换的两种实现方式

**方式一：iptables MASQUERADE（当前方案）**
- 优点：成熟稳定，不需要改代码
- 缺点：需要额外配置 iptables

**方式二：内化到 TUN 逻辑（待实现）**
- Phaethon 发包前自动把源 IP 改成旁路网关 IP
- 优点：配置更简单，不需要 iptables
- 缺点：需要改代码

### 关于 iptables 规则的讨论

**不需要的规则**：
- ~~PREROUTING 标记 + 策略路由~~：直接用默认路由指 TUN 即可
- ~~排除代理服务器目标地址~~：不需要，流量进 TUN 后由 Phaethon 决定如何处理

**需要的规则**：
- POSTROUTING MASQUERADE：源 IP 替换（除非内化到 TUN）

### 二层转发与企业交换机安全

旁路网关场景下，客户端主动把网关指向旁路网关 IP：
- 交换机查 MAC 地址表 → 找到旁路网关 MAC → 正常转发
- 这是正常的三层路由行为，不是 ARP 欺骗

**可能的拦截**（企业交换机高级安全功能，需手动开启）：
- DHCP Snooping + DAI：检查 IP-MAC 绑定
- IP Source Guard：限制端口源 IP
- Port Security：限制端口 MAC 数量
- 802.1X：端口认证

**结论**：大部分环境只开基本二层转发，不检查三层。只要客户端主动指向旁路网关，不会被拦截。

### DNS 方案（已验证）

#### 架构

DNSHijacker 是内嵌在 gVisor netstack 里的 DNS 服务器，绑定专用 IP `192.0.2.3:53`。系统 DNS 指向该 IP，查询通过 TUN 路由自然进入 netstack。

```
应用 DNS 查询 → 系统 DNS (192.0.2.3)
    │
    ▼
路由到 TUN → gVisor netstack
    │
    ▼
DNSHijacker (192.0.2.3:53) 返回 Fake-IP (198.18.x.x)
```

#### Windows TUN 模式验证结果

- DNSHijacker 绑定 192.0.2.3:53 ✓
- 系统 DNS 指向 192.0.2.3 ✓
- `nslookup baidu.com` 返回 Fake-IP ✓
- DIRECT 流量正常 (HTTP 200) ✓

#### 已删除的 DNSProxy

原 Windows 实现使用 DNSProxy（主机侧 OS socket 监听 192.0.2.2:53，桥接到 DNSHijacker）。该方案占用主机 53 端口，对旁路网关不可行。已在代码中彻底删除。

#### 旁路网关 DNS

旁路网关直接复用同一架构：
- 系统 DNS 设为 192.0.2.3
- 客户端 DHCP 下发 DNS 为 192.168.0.100（旁路网关 LAN IP）
- 旁路网关上通过 iptables 把 53 端口流量 DNAT 到 192.0.2.3，或直接让客户端 DNS 指向 192.0.2.3

## 与原 Phaethon 的关系

- 原 Phaethon: `/root/phaethon` + `/root/config.yaml` (端口 39999)
- 旁路 Phaethon: `/root/bypass-gateway/` (端口 39998)
- 两者独立运行，互不影响
- 共用同一个二进制文件

## 待完成

1. ~~DNS 架构改造~~ ✓ 已完成并验证
2. ~~删除 DNSProxy~~ ✓ 已删除
3. 编译 Linux 二进制并部署到 QG
4. 配置旁路网关 config.yaml
5. 配置 iptables (MASQUERADE) + 开启 IP 转发
6. 配置客户端测试
7. 可选：源 IP 替换内化到 TUN 逻辑
8. 可选：安装 dnsmasq 提供 DHCP 服务
