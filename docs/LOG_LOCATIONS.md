# Phaethon 日志位置文档

## 日志文件位置

### QG 环境 (10.11.61.41)
- **主日志**: `/root/data/logs/phaethon.log`
  - 程序自己写的滚动日志（10MB 轮转）
  - 包含所有 util.LogInfo/LogDebug 日志
  - **查日志优先看这个**
  
- **gVisor stdout**: `/root/phaethon.log`
  - gVisor 诊断日志（fmt.Fprintf to stderr）
  - 包含 [STACK-NIC], [QDISC], [IPIP-ENCAP], [GVISOR-TRACE] 等
  - **gVisor 层面的诊断看这个**

### VM 环境 (10.21.20.65)
- **主日志**: `C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log`
  - 程序自己写的滚动日志（10MB 轮转）
  - 包含所有 util.LogInfo/LogDebug 日志
  - **查日志优先看这个**
  
- **gVisor stdout**: `C:\Users\Docker\Desktop\Workspace\phaethon\phaethon-stdout.log`
  - gVisor 诊断日志（fmt.Fprintf to stderr）
  - 包含 [STACK-NIC], [QDISC], [IPIP-ENCAP], [GVISOR-TRACE] 等
  - **gVisor 层面的诊断看这个**
  - **注意**: 此文件可能增长到很大（21GB），需要定期清理

### JF 环境 (36.140.28.178)
- **主日志**: `/home/layer4/layer4-deploy/data/logs/phaethon.log`
- **辅助日志**: `/home/layer4/layer4-deploy/phaethon.log` (nohup stdout)

### GG 环境 (106.13.183.103)
- **主日志**: `/home/layer4/phaethon-gg/data/logs/phaethon.log`
- **辅助日志**: `/home/layer4/phaethon-gg/phaethon.log` (nohup stdout)

## 日志类型

### 1. 应用层日志 (util.Log*)
- 位置: 主日志文件
- 格式: `[phaethon] 2026/10/13 12:50:46 [INFO] [TAG] message`
- 包含: [LINKNIC-WRITEPACKETS], [P2P-SENDER-DIAG], [HTUNNEL-GET-DIAG], [MESH-TRACE] 等

### 2. gVisor 诊断日志 (fmt.Fprintf to stderr)
- 位置: gVisor stdout 文件
- 格式: `[TAG] message`
- 包含: [STACK-NIC], [QDISC], [IPIP-ENCAP], [GVISOR-TRACE], [ROUTE-SELECTOR] 等

## 日志清理

### VM 环境日志清理
```powershell
# 停止服务
Stop-ScheduledTask -TaskName PhaethonTUN
Stop-Process -Name phaethon -Force

# 清理 gVisor stdout（可能很大）
cmd /c "echo. > C:\Users\Docker\Desktop\Workspace\phaethon\phaethon-stdout.log"

# 启动服务
Start-ScheduledTask -TaskName PhaethonTUN
```

## 调试技巧

1. **优先查主日志**: 大部分问题在主日志中能看到
2. **gVisor 问题查 stdout**: IPIP 封装、路由决策等 gVisor 层面的诊断在 stdout 日志
3. **实时查看**: 
   - QG: `tail -f /root/data/logs/phaethon.log` 或 `tail -f /root/phaethon.log`
   - VM: `Get-Content ... -Tail 50 -Wait`
4. **搜索特定流量**: 用 grep/Select-String 搜索 IP 地址或协议号
