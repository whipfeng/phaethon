# QG 旁路网关方案

## 状态：已部署

目录：`/root/bypass-gateway/`

## 概述

QG 环境 (10.11.61.40 / 192.168.1.101) 作为旁路网关，为局域网设备提供透明代理。

## 网络拓扑

```
[主路由 192.168.1.1]
       |
       | LAN
       |
[QG 旁路网关 192.168.1.101]
  - eth1: 192.168.1.101 (旁路网关接口)
  - eth0: 10.0.2.15 (管理口，SSH 访问)
  - Phaethon TUN 模式
       |
[局域网设备]
  网关 → 192.168.1.101
```

## 目录结构

```
/root/bypass-gateway/
├── README.md           # 使用说明
├── config/
│   └── phaethon.yaml   # 旁路专用配置 (admin:39998, TUN 模式)
├── scripts/
│   ├── start.sh        # 启动旁路网关
│   ├── stop.sh         # 停止旁路网关
│   └── iptables.sh     # iptables 规则
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

## 工作原理

1. **流量入口**：局域网设备网关指向 192.168.1.101
2. **iptables 重定向**：PREROUTING 将 TCP/UDP 流量重定向到 TUN 接口
3. **Phaethon TUN**：处理流量，走代理规则
4. **NAT 出口**：POSTROUTING MASQUERADE 做源地址转换

## 与原 Phaethon 的关系

- 原 Phaethon: `/root/phaethon` + `/root/config.yaml` (端口 39999)
- 旁路 Phaethon: `/root/bypass-gateway/` (端口 39998)
- 两者独立运行，互不影响
- 共用同一个二进制文件

## 待完成

1. 测试 TUN 模式是否正常工作
2. 验证 iptables 规则
3. 配置客户端测试
4. 可选：安装 dnsmasq 提供 DHCP 服务
