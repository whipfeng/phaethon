# 术语表

| 术语 | 英文 | 说明 |
|------|------|------|
| 入站 | Inbound / Server | 外部客户端连接到 phaethon 的入口协议，如 SOCKS5、Trojan、h_tunnel 等 |
| 出站 | Outbound / Dialer | phaethon 连接到目标或下一跳代理时使用的协议 |
| 代理链 | Proxy Chain | 出站拨号按 `proxy` 字段逐级转发，形成链式路径 |
| 代理组 | Proxy Group | 包含多个候选成员的选择器，类型有 `select` / `best` / `load-balance` |
| 手动成员 | Manual Member | 代理组 `proxies` 字段中显式列出的代理或嵌套组 |
| 订阅成员 | Subscription Member | 由 `subscription` + `subscription-filter` 从订阅节点池筛选出的成员 |
| 活动成员 | Active Member | 当前生效的成员；`select` 组持久化为 `active-member`，其他类型运行时计算 |
| 健康检查 | Health Check | 对代理节点执行可达性/延迟探测，结果用于 `best` / `load-balance` 选择 |
| 反向连接 | Reverse Tunnel | 内网被动端主动向注册端注册连接，公网主动端复用该连接传输数据 |
| 注册端 | Registry | 运行 `reverse/Registry` 的服务端，维护反向连接池并匹配主动端请求 |
| 反向端 | Reverse Client / Passive | 内网侧主动维持到 Registry 的 BIND 连接 |
| 四角色拓扑 | Four-Role Topology | 客户端 → 注册端 → 反向端 → 服务端的线性链式模型 |
| TUN 模式 | TUN Mode | 通过系统 TUN 接口 + gvisor 网络栈拦截系统流量的模式 |
| Mapping | Mapping | 入站端口监听配置，定义某个端口使用哪种协议提供服务 |
| Rule | Rule | 路由规则，按目标地址决定流量走哪个代理或代理组 |
| Resolver | Resolver | 地址重写规则，类似 hosts |
| 配置目标 | Config Target | Admin 保存配置时的目标：`base`（rule.yaml）或 `env`（rule-{env}.yaml） |
| NodeViewer | NodeViewer | Admin UI 中用于查看/筛选订阅节点池的弹窗组件 |
