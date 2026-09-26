# Phaethon Project Instructions

## 铁律（不可违反）

### 1. MDD (Markdown-Driven Development) 开发流程

本项目采用 MDD 体系，**所有功能开发必须遵循以下流程**：

```
inputs/ (原始需求)
   │
   ▼  提炼 & 评审
specs/ (规格冻结)
   │
   ▼  设计 & 评审
plans/ (架构方案，含 ADR)
   │
   ▼  拆解
tasks/ (原子化工单)
   │
   ▼  实现
code (生产代码)
```

**文档位置**（项目仓库内）：
| 目录 | 用途 | 示例 |
|------|------|------|
| `docs/inputs/` | 原始需求、故障分析 | `reverse-udp-topology-bug-analysis.md` |
| `docs/specs/` | 规格定义 | `tun_spec.md`, `config_spec.md` |
| `docs/plans/` | 架构设计 | `mesh_multi_vip_design.md` |
| `docs/tasks/` | 开发任务 | `tun-tasks.md` |

**注意**：`.qoder/plans/` 是 Qoder CLI 的内部目录，**不是**项目的 MDD 文档位置。

### 2. 严格按设计方案执行
设计方案（`docs/plans/` 中的设计文档）确定后，必须严格按方案实现，不得擅自更改方案。

### 3. MDD 冲突处理流程（强制）
定位问题时，如果发现与设计文档冲突或设计中未定义的行为：
1. **停下来**：不要自行修改代码或方案
2. **报告冲突**：向用户说明发现的具体冲突点，引用设计文档原文
3. **等待授权**：等用户明确授权后再修改方案
4. **先改方案**：修改 `docs/plans/` 中的设计文件，获得用户确认
5. **再改代码**：方案确认后，再动手修改代码

**绝对禁止**：发现冲突后直接改代码而不更新方案，或先改代码再补方案。

### 4. 实现前检查清单
开始实现功能前，确认：
- [ ] 设计文档已在 `docs/plans/` 中
- [ ] 任务文档已在 `docs/tasks/` 中（可选，复杂功能需要）
- [ ] 已获得用户明确授权开始实现

### 5. Netstack 保护规则（强制）

**gVisor netstack 是核心基础设施，任何修改必须谨慎**：

**禁止行为**：
- ❌ 不得擅自修改现有 mesh endpoint 的行为
- ❌ 不得改变 netstack 的路由逻辑
- ❌ 不得影响现有的 P2P 连接
- ❌ 不得破坏 mesh 网络的稳定性
- ❌ 不得修改 netstack 的初始化流程

**如果需要调整方案**：
1. **停下来**：不要自行修改代码
2. **报告影响**：向用户说明需要调整的原因和影响范围
3. **等待授权**：必须获得用户明确同意
4. **记录变更**：将调整记录到本文件（agents.md）
5. **更新方案**：修改 `docs/plans/` 中的设计文档
6. **再改代码**：方案确认后，再动手修改代码

**新增功能原则**：
- ✅ 优先复用现有 netstack 能力
- ✅ 新增 endpoint 不影响现有 endpoint
- ✅ 新增路由规则不覆盖现有路由
- ✅ 新增协议处理器不干扰现有处理器

**h_tunnel 约束**：
- h_tunnel 类型代理必须启用 P2P（下行依赖 mesh P2P 网络）
- Admin UI 中 h_tunnel 的 P2P 复选框应禁用，强制启用
- 配置验证：h_tunnel 且 P2P=false 时应拒绝

### 6. MDD 文档索引
| 文档 | 路径 | 用途 |
|------|------|------|
| 文档管理规范 | `docs/specs/doc_management_spec.md` | MDD 流程详细说明 |
| 文档索引 | `docs/index.md` | 所有文档的索引入口 |
| 项目指令 | `agents.md` | 本文件，项目规范 |

**定位问题时的检查顺序**：
1. 先查 `docs/plans/` 是否有相关设计文档
2. 如果设计文档有定义但代码行为不同 → 报告冲突，等待授权
3. 如果设计文档未定义 → 报告缺失，等待用户补充方案
4. 获得授权后，先更新设计文档，再改代码

## 项目概述

Phaethon 是一个 Go 语言实现的网络代理/TUN 隧道工具，支持 Fake-IP DNS、规则路由、TCP/UDP 转发等功能。

- **模块名**: `phaethon`
- **Go 版本**: 1.25.0
- **Git 仓库**: `https://github.com/whipfeng/phaethon.git`，分支 `master`
- **核心模块**: `tun/`（TUN 隧道）、`dialer/`（拨号器）、`server/`（服务端）、`reverse/`（反向代理）

## 环境信息

### QG环境 (10.11.61.40)
- **别名**: QG环境
- **本地访问**: `192.168.1.100`（可直接从本地访问）
- **远程访问**: `10.11.61.40`
- **SSH**: `ssh -p 2222 root@10.11.61.40`（免密登录）
- **SSH 备用地址**: `ssh -p 2222 root@192.168.1.100` 或 `ssh -p 2222 root@192.168.1.101`（LAN IP，当 WAN SSH 不可用时使用，需在同一局域网）
- **Layer4 服务**: 运行在 `/root/` 目录，二进制 `layer4-go-alpine`，配置文件 `/root/conf/rule.yaml`
- **Admin API**: `:39999`（需启用）
- **用途**: 代理服务环境，提供 SOCKS5/HTTP 代理映射

### VM环境 (10.21.20.65)
- **别名**: VM环境
- **SSH**: `ssh -p 39022 Docker@10.21.20.65`（用户是 Docker，不是 Administrator）
- **工程目录**: `C:\Users\Docker\vs-project\workspace\phaethon\`
  - 源码目录，用于编译和开发
- **部署目录**: `C:\Users\Docker\Desktop\Workspace\phaethon\`
  - 运行产物: `phaethon.exe`、`phaethon-watchdog.exe`、`config.yaml`、`wintun.dll`
  - 日志: `phaethon-stdout.log`、`phaethon-watchdog.log`
  - 计划任务: `PhaethonTUN`（启动 phaethon.exe）
  - Admin API: `:39999`

### 本地开发环境
- **路径**: `/Users/cmll/IdeaProjects/phaethon/`
- 从 Windows VM 同步代码（git push/pull）

## 构建与部署

### 部署经验总结（重要！）
- **VM 环境**：SCP 不能直接上传到 Windows 长路径（会损坏文件），必须先上传到 `/c/temp/` 等简单路径，再用 PowerShell 复制
- **VM 环境**：部署必须在一条 SSH 命令中完成 stop + replace + start，否则会触发部署安全检查 hook
- **MS9/MS10 环境**：本地直连 SSH（`ssh migu@10.161.88.9/10`），无需通过 GG 环境中转，不要 ping 测试
- **文件验证**：上传后用 `md5sum` 验证文件哈希，确保完整性

### 部署顺序规则（重要！）
- **先部署 VM 环境**（Windows 测试环境），等用户确认没问题后，**再部署 QG 环境**（生产服务环境）
- 不要同时部署两个环境，必须分步验证

### 构建命令
- Windows: `.\build.ps1 windows` 或 `make windows`
- Linux: `make linux`
- 交叉编译: `make all`（linux, linux-arm64, windows, windows7, windows-arm64, darwin-amd64, darwin-arm64）

### 各环境部署

#### QG环境 (10.11.61.40) — 主服务环境
- **编译**: `make linux`
- **上传**: `scp dist/linux-amd64/phaethon root@10.11.61.40:/root/phaethon`（通过 SSH 端口 2222）
- **启动**: `ssh -p 2222 root@10.11.61.40 "rc-service phaethon start"`
- **停止**: `ssh -p 2222 root@10.11.61.40 "rc-service phaethon stop"`
- **重启**: `ssh -p 2222 root@10.11.61.40 "rc-service phaethon restart"`
- **SSH 备用**: 当 `10.11.61.40` 不可达时，依次尝试 `192.168.1.100` 或 `192.168.1.101`（LAN IP，需同网段），端口和用户不变
- **配置文件**: `/root/config.yaml`
- **二进制路径**: `/root/phaethon`
- **日志**: `/root/phaethon.log`
- **管理**: OpenRC supervise-daemon，自动拉起
- **TUN**: 已启用（`tun: enabled: true`）
- **旁路网关**: 已启用（`tun: bypass-gateway: true`，TUN 启动后自动添加 iptables FORWARD 规则，LAN 机器可配 gateway=192.168.1.101 DNS=192.0.2.3 走代理）

#### VM环境 (10.21.20.65) — Windows 开发/测试环境
- **编译**: `make windows`
- **上传**: **不要直接 SCP 到长路径！** 先上传到 `/c/temp/`，再用 PowerShell 复制：
  ```bash
  # 1. 上传到临时路径
  scp -P 39022 dist/windows-amd64/phaethon.exe Docker@10.21.20.65:/c/temp/phaethon.exe
  # 2. 验证哈希（确保文件完整）
  ssh -p 39022 Docker@10.21.20.65 "md5sum /c/temp/phaethon.exe"
  # 3. 停止、替换、启动（一条命令）
  ssh -p 39022 Docker@10.21.20.65 "powershell -Command 'Stop-ScheduledTask -TaskName PhaethonTUN; Stop-Process -Name phaethon -Force; Start-Sleep -Seconds 3; Copy-Item -Force \"C:\temp\phaethon.exe\" \"C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe\"; Start-ScheduledTask -TaskName PhaethonTUN'"
  ```
- **重要**: SCP 直接上传到 `C:\Users\Docker\Desktop\Workspace\phaethon\` 等长路径会导致文件损坏（哈希不一致）。必须先上传到简单路径（如 `/c/temp/`），再用 PowerShell 复制
- **启动**: `ssh -p 39022 Docker@10.21.20.65 "powershell -Command \"Start-ScheduledTask -TaskName PhaethonTUN\""`
- **停止**: `ssh -p 39022 Docker@10.21.20.65 "powershell -Command 'Stop-Process -Name phaethon -Force'"`
- **部署**: 必须在一条命令中完成 stop + replace + start，否则会触发部署安全检查 hook
- **配置文件**: `C:\Users\Docker\Desktop\Workspace\phaethon\config.yaml`
- **二进制路径**: `C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe`
- **日志**: stdout 直接输出到控制台（VBS 隐藏窗口启动，无日志重定向文件）；调试日志在 `test-stderr.log`、`mesh-stderr.log` 等（手动启动时产生）
- **启动方式**: 计划任务 `PhaethonTUN` → `wscript.exe start-phaethon.vbs` → `phaethon.exe`（隐藏窗口，交互式桌面会话）
- **VBS 脚本**: `start-phaethon.vbs` 内容为 `WshShell.Run "phaethon.exe", 0, False`（0=隐藏窗口, False=异步）
- **重要**: 必须通过计划任务或 VBS 启动，**不能通过 SSH 直接 Start-Process 启动**。phaethon 需要在 Docker 用户的交互式桌面会话中运行，因为 TUN 驱动（Wintun）和 UAC 提权依赖桌面会话上下文。SSH 启动会导致进程 ~2 秒后 hard crash（无 Go panic，疑似 access violation）
- **看门狗**: 已合并到主程序（`PHAETHON_WORKER` 环境变量区分 watchdog/worker 模式），启动后自动产生 2 个进程（watchdog + worker）
- **TUN**: 已启用（`tun: enabled: true`，使用 Wintun 驱动）
- **旁路网关**: 未启用（Windows 环境，无 iptables FORWARD）
- **磁盘上的二进制文件**:
  - `phaethon.exe` — 当前运行的主程序
  - `phaethon-new.exe` — 新版二进制（待部署）
  - `phaethon.old.exe` — 旧版备份（可删除）
  - `phaethon-watchdog-watchdog.exe` — 非常旧的独立看门狗（应删除，已合并到主程序）

#### JF环境 (36.140.28.178) — H_Tunnel 服务端
- **编译**: `make linux`
- **上传**: `scp -P 60002 dist/linux-amd64/phaethon layer4@36.140.28.178:/home/layer4/layer4-deploy/phaethon`
- **启动**: `ssh -p 60002 layer4@36.140.28.178 "chmod +x /home/layer4/layer4-deploy/phaethon && cd /home/layer4/layer4-deploy && nohup ./phaethon > phaethon.log 2>&1 &"`
- **停止**: `ssh -p 60002 layer4@36.140.28.178 "ps aux | grep phaethon | grep -v grep | awk '{print \$2}' | xargs kill"`
- **⚠️ 部署必须确保成功**：部署 JF 时必须在同一命令内完成停止+替换+启动，部署后必须验证：进程存在（watchdog+worker 两个进程）、端口 32457/39999 处于 LISTEN、admin 接口返回 200。验证失败要立即修复。
- **禁止用 `-version` 等参数直接运行二进制**：phaethon 无 version 参数，任何参数都会当作正常启动，产生重复实例抢占端口
- **配置文件**: `/home/layer4/layer4-deploy/config.yaml`
- **二进制路径**: `/home/layer4/layer4-deploy/phaethon`
- **日志**: `/home/layer4/layer4-deploy/phaethon.log`
- **注意**: 无 systemd/OpenRC，手动 nohup 启动
- **TUN**: 未启用（此环境仅做 h_tunnel 服务端）
- **旁路网关**: 未启用

## 进程管理规则（重要）

- **看门狗已合并到主程序**：`phaethon.exe` 启动后自动产生 2 个进程（watchdog 父进程 + worker 子进程），通过 `PHAETHON_WORKER` 环境变量区分。不再有独立的 `phaethon-watchdog.exe`
- **VM 启动必须用计划任务**：`Start-ScheduledTask -TaskName PhaethonTUN`（通过 VBS 在交互式桌面会话中启动）。**禁止通过 SSH 直接 Start-Process 启动**，会因缺少桌面会话上下文导致 hard crash
- **VM 停止**: `Stop-Process -Name phaethon -Force`（会杀掉所有 phaethon.exe 进程，包括 watchdog 和 worker）
- **VM 重启**: 先 Stop-Process，再 Start-ScheduledTask
- **QG 环境**: 使用 `rc-service phaethon restart`，OpenRC 自动拉起
- **VM 和 QG 环境不能同时关闭**：必须确保一个是正常运行状态后，才能操作另外一个。这是为了保证始终有一个可用的测试/生产环境。

## 技术架构

### TUN 引擎 (`tun/`)
- **Wintun**: Windows TUN 驱动，使用共享内存环形缓冲区（DLL 调用）
- **gVisor netstack**: 用户态 TCP/IP 协议栈，处理数据包
- **channel.Endpoint**: gVisor 链路层端点，512 包容量，非阻塞写入
- **Fake-IP**: DNS 域名→mesh subnet 虚拟 IP 映射（mesh 模式下从节点 subnet 分配）
- **TCP Forwarder**: gVisor `tcp.Forwarder`，maxInFlight=1024，每个连接在独立 goroutine 处理
- **UDP Forwarder**: 自定义 UDP 转发，带空闲超时

### 关键数据流
```
Windows 应用 → Wintun 驱动 → readLoop → InjectInbound → gVisor netstack
                                                         ↓
                                              TCP/UDP Forwarder → 代理连接

gVisor netstack → writeLoop → Wintun 驱动 → Windows 应用
```

### 已知问题
- **TUN Read Stall**: `WintunReceivePacket()` DLL 调用在运行 4-30 秒后可能永久阻塞
  - 详见 `docs/issues/tun-read-stall.md`
  - 修复方向: 使用 `ReadWaitEvent` + `WaitForSingleObject` 超时检测 + 自动恢复

## Mesh 域名访问机制（重要！）

节点间通过 mesh 网络互访 admin/API 时，**直接用域名、不带端口**（如 `https://gg.phn/api/xxx`），不要带 `:39999` 之类的端口。

### 完整链路（已实测验证）

```
发起方 curl https://gg.phn/api/packages
  1. DNS: gg.phn → mesh DNS 应答目标节点 subnet 内地址（如 gg.phn → 100.179.0.10、jf.phn → 100.2.0.11）
     不是 100.0.0.x fakeIP，是目标节点 subnet 内的真实可路由 mesh 地址
  2. 路由: 100.179.0.10 走 mesh 路由表 → P2P 链路（可中继）→ 到达 GG 节点
  3. 交付: GG 的 HandleMeshFrame → InjectMeshPacket → gVisor netstack
  4. 处理: netstack 用 fakeIP 表还原域名 == 本节点域名（gg.phn）
     → localNodeDomain=true → adminHandler.ServeConn() 直接处理
     （tun/engine.go:2042，绕过 OS 网络栈，端口无关）
```

### 关键结论

- **端口无关**：目标节点上任何端口（443、12345、39999）都由 admin handler 接管，OS 层 listener（如 GG 39999 的 trojan）与 mesh 路径互不干扰
- **不要在 URL 里写端口**：`https://<node>.phn/api/xxx` 即可（默认 443）
- **证书是自签的**：客户端必须 `InsecureSkipVerify: true`，否则报 `tls: unknown certificate`（GG 日志里大量此错误来自浏览器访问）
- **与旁路网关（bypass-gateway）的关系**：QG 的 bypass-gateway 是 iptables FORWARD 让 LAN 机器借道代理；节点互访走的是上面 netstack 直连 admin handler 的路径，两者是不同机制

## 远程浏览器验证

远程 Windows VM 上安装了 qoder CLI，可以通过 SSH 调用它来操控 VM 上的浏览器进行页面验证。

```bash
ssh -p 39022 Docker@10.21.20.65 "qoder -p '打开浏览器访问 <URL>，等页面加载完成后截图保存到 C:\Users\Docker\Desktop\<name>.png，然后总结页面内容' --yolo"
```

- qoder 路径: `C:\Users\Docker\.qoder\entry\qoder.cmd`
- 使用 `--yolo` 跳过权限确认
- 截图保存到远程桌面，可用 scp 拉回本地查看

## Git Push（DNS 污染备用方案）

本地 DNS 可能污染 `github.com`，导致 `git push` 失败。此时用 GitHub 真实 IP 推送：

```bash
GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=no -o HostKeyAlgorithms=ssh-ed25519,ssh-rsa' \
  git push git@140.82.114.4:whipfeng/phaethon.git master
```

## QG 环境服务化配置（重要！）

**QG 环境（10.11.61.40，SSH 端口 2222）已服务化部署，不要随意关闭！**

### 服务信息
- **系统**: Alpine Linux 3.17 + OpenRC
- **服务脚本**: `/etc/init.d/phaethon`
- **开机启动**: 已配置（default runlevel）
- **工作目录**: `/root/`
- **配置文件**: `/root/config.yaml`（已从旧 layer4 整合 BASE+ENV：9 proxies, 17 mappings, 197 rules）
- **日志文件**: `/root/phaethon.log`
- **PID 文件**: `/root/phaethon.pid`
- **健康监控**: `/root/monitor-proxy.sh`（每 2 分钟 cron 检查）

### 管理命令
```bash
# 启动
rc-service phaethon start

# 停止（谨慎！）
rc-service phaethon stop

# 重启
rc-service phaethon restart

# 状态
rc-service phaethon status

# 查看日志
tail -f /root/phaethon.log
```

### 注意事项
- **不要直接 kill 进程**，使用 `rc-service` 命令
- 这是生产服务，重启前需要确认
- 监控脚本会检测进程、端口、代理功能
- 旧 layer4 仍在宿主机运行（端口 39999），和 QG 虚拟机是两个独立环境

## JF环境 (36.140.28.178)

### SSH 访问
- **别名**: JF环境
- **SSH**: `ssh -p 60002 layer4@36.140.28.178`
- **内网 IP**: 10.140.51.2
- **系统**: CentOS/RHEL Linux

### Phaethon 服务
- **工作目录**: `/home/layer4/layer4-deploy/`
- **二进制**: `/home/layer4/layer4-deploy/phaethon`（最新 Linux amd64）
- **配置文件**: `/home/layer4/layer4-deploy/config.yaml`
- **日志**: `/home/layer4/layer4-deploy/phaethon.log`
- **启动方式**: `cd /home/layer4/layer4-deploy && nohup ./phaethon > phaethon.log 2>&1 &`
- **H_Tunnel 端口**: 32457（通过 NAT 18080 转发，经 nginx 8080 代理到 32457）
- **Admin API**: 39999
- **TUN**: 未启用（此环境只做 h_tunnel 服务端）

### Nginx 反向代理
- 监听 8080 和 7781 端口
- `/ui/resdata/` 代理到 upstream `resdatabackend`（10.140.51.2:32457）— h_tunnel 协议
- `/layer/` 代理到 10.140.51.2:39999 — Admin API
- 18080 端口由上游 NAT 设备转发到 8080

### 其他服务
- Elasticsearch, Java 应用 `maas-ant-0.1.0.jar`
- Harbor: `/opt/software-yaxin/harbor/`
- KubeKey: `/opt/software-yaxin/kk/`

### MGMS_HT 代理配置（QG 客户端侧）
```yaml
- name: MGMS_HT
  type: h_tunnel
  server: 36.140.28.178
  port: 18080
  url: http://36.140.28.178:18080/ui/resdata/
  password: "3245930005"
```
- 密码用于加密 h_tunnel 协议头，防止 NAT 设备 DPI 拦截

### 使用 MGMS_HT 作为 via 的代理
- `GGSV_TJ`（trojan, via: MGMS_HT）
- `MGMS_TJ`（trojan, via: MGMS_HT）
- `GGDYSV_TJ`（trojan, via: MGMS_HT）

---

## GG环境 (106.13.183.103)

### SSH 访问
- **别名**: GG环境
- **SSH**: `ssh layer4@106.13.183.103`
- **系统**: Linux

### Phaethon 服务
- **工作目录**: `/home/layer4/phaethon-gg/`（**必须从此目录启动**）
- **二进制**: `/home/layer4/phaethon-gg/phaethon`
- **配置文件**: `/home/layer4/phaethon-gg/config.yaml`
- **日志**: `/home/layer4/phaethon-gg/phaethon.log`
- **部署命令**:
  ```bash
  # 上传新版二进制到临时位置
  scp dist/linux-amd64/phaethon layer4@106.13.183.103:/home/layer4/phaethon-gg/phaethon-new
  # 停止、替换、启动（必须在工作目录执行）
  ssh layer4@106.13.183.103 "pkill -9 phaethon; sleep 2; cd /home/layer4/phaethon-gg && cp phaethon-new phaethon && nohup ./phaethon >> phaethon.log 2>&1 &"
  ```
- **启动方式**: `cd /home/layer4/phaethon-gg && nohup ./phaethon >> phaethon.log 2>&1 &`（watchdog 模式，自动重启）
- **⚠️ 重要**: 
  - **必须**在 `/home/layer4/phaethon-gg/` 目录下运行 `./phaethon`
  - **不要**部署到 `/home/layer4/phaethon`（这是另一个实例，使用不同的配置）
  - 错误目录启动会导致使用错误的配置文件，反向连接等功能会失效
- **Admin API**: 39998 (HTTPS)
  - 用户: admin
  - 密码: changeme
- **Trojan 映射**: 端口 39999
  - 密码: Dynamic39*09
  - SNI: www.dynamictj.com
- **TUN**: 未启用
- **Mesh**: 已启用
  - node-id: 15538109650403028488
  - subnet: 100.179.0.0/16

### 其他端口（反向代理映射）
- 39008, 39009, 39010, 39012, 39080（通过 registry-proxy 反向映射到内网服务）
- 这些端口由 QG 环境配置，通过 GG 环境的 registry 注册并提供访问

### 注意事项
- 从旧版 layer4-go-verify 迁移而来
- 使用 watchdog 模式运行，进程崩溃会自动重启

---

## MS9环境 (10.161.88.9)

### SSH 访问
- **别名**: MS9环境
- **SSH**: `ssh migu@10.161.88.9`（**本地直连**，无需通过 GG 环境）
- **系统**: CentOS/RHEL Linux (kernel 5.13.19)
- **用户**: migu
- **重要**: 直接从本地 SSH 连接，不要 ping 测试，不要用 ProxyCommand 通过 GG 转发

### Phaethon 服务
- **工作目录**: `/home/migu/phaethon/`
- **二进制**: `/home/migu/phaethon/phaethon`
- **配置文件**: `/home/migu/phaethon/config.yaml`
- **日志**: `/home/migu/phaethon/logs/phaethon.log` 和 `/home/migu/phaethon/.phaethon/phaethon.log`
- **启动方式**: `cd /home/migu/phaethon && nohup ./phaethon > logs/phaethon.log 2>&1 &`（watchdog 模式）
- **Admin API**: 39999（已启用，无认证）
- **TUN**: 未启用
- **Mesh**: 已启用
  - node-id: ms9
  - VIP: 100.189.0.1
  - subnet: 100.189.0.0/16

### 代理配置
- **连接方式**: 通过 GG 环境的 MGMS_TJ 代理
  - server: 106.13.183.103:39017
  - SNI: www.mgmstj.com
  - password: Trojan77585*3
- **反向注册**: RV_PROXY (reverse-address: 10.161.88.9)
- **用途**: 通过 mesh 网络访问其他节点（qg, vm, jf, gg 等）

### 网络拓扑
```
ms9 (100.189.0.1) → GG (MGMS_TJ) → 其他节点
  - gg.phn (hop=1)
  - qg.phn (hop=2)
  - vm.phn (hop=2)
  - jf.phn (hop=3)
  - qgw.phn (hop=3)
```

### 注意事项
- **SSH 访问**：本地直连 `ssh migu@10.161.88.9`，无需通过 GG 环境中转
- 部署时已通过 mesh 网络自动发现其他节点
- RV_PROXY 的 P2P 连接失败是正常的（反向代理无 server 地址）

---

## MS10环境 (10.161.88.10)

### SSH 访问
- **别名**: MS10环境
- **SSH**: `ssh migu@10.161.88.10`（**本地直连**，无需通过 GG 环境）
- **系统**: CentOS/RHEL Linux (kernel 5.13.19)
- **用户**: migu
- **重要**: 直接从本地 SSH 连接，不要 ping 测试，不要用 ProxyCommand 通过 GG 转发

### Phaethon 服务
- **工作目录**: `/home/migu/phaethon/`
- **二进制**: `/home/migu/phaethon/phaethon`
- **配置文件**: `/home/migu/phaethon/config.yaml`
- **日志**: `/home/migu/phaethon/logs/phaethon.log` 和 `/home/migu/phaethon/.phaethon/phaethon.log`
- **启动方式**: `cd /home/migu/phaethon && nohup ./phaethon > logs/phaethon.log 2>&1 &`（watchdog 模式）
- **Admin API**: 39999（已启用，无认证）
- **TUN**: 未启用
- **Mesh**: 已启用
  - node-id: ms10
  - VIP: 100.96.0.1
  - subnet: 100.96.0.0/16

### 代理配置
- **连接方式**: 通过 GG 环境的 MGMS_TJ 代理
  - server: 106.13.183.103:39017
  - SNI: www.mgmstj.com
  - password: Trojan77585*3
- **反向注册**: RV_PROXY (reverse-address: 10.161.88.10)
- **用途**: 通过 mesh 网络访问其他节点（qg, vm, jf, gg, ms9 等）

### 网络拓扑
```
ms10 (100.96.0.1) → GG (MGMS_TJ) → 其他节点
  - gg.phn (hop=1)
  - ms9.phn (hop=2)
  - qg.phn (hop=2)
  - vm.phn (hop=2)
  - jf.phn (hop=3)
  - qgw.phn (hop=3)
```

### 注意事项
- **SSH 访问**：本地直连 `ssh migu@10.161.88.10`，无需通过 GG 环境中转
- 部署时已通过 mesh 网络自动发现其他节点
- RV_PROXY 的 P2P 连接失败是正常的（反向代理无 server 地址）
