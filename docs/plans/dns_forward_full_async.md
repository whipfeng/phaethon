# DNS 转发全异步化（worker 零等待）

## 背景

排查 jf DNS "时通时不通"（见 [[project-gg-path-instability]]）时发现转发路径的结构性缺陷：

v0.2.0 引入异步转发器（`mesh/dns_async_forwarder_design.md`）的初衷是 worker 发完即走、
由后台 listener 收包后直接回客户端。但实现漂移成了"异步发送 + **worker 同步等回调
5 秒**"（`forwardToRemote` 里 `select` 等 `resultCh`，dns.go:651-666）。

致命放大链：

```
远程转发变慢/丢包 → 4 个 dnsWorkerLoop 被 5s 超时逐个占住
→ dnsQueryCh（1024）填满 → serveLoop 阻塞在投递，不再 udpEP.Read
→ netstack GIP:53 接收队列积压丢包 → 新查询连进入机会都没有
→ 本地 fakeIP/缓存应答也排不进去 → 整个 :53 周期性假死
→ worker 超时释放后 Drain 恢复 → 再堵 → "时通时不通"
```

转发/代理/控制台等不走这条 channel 的服务全程正常，与观测吻合
（jf 事故期间仅 DNS 失声，admin/socks/转发健在）。

## 设计

worker 对远程转发路径**零等待**；listener 收包后直接回客户端；超时由 listener 内
定时清扫承担。

### 数据流（改造后）

```
serveLoop ──dnsQueryCh──▶ worker: processQuery
                              │ 命中缓存/AAAA/本地 pool → 直接 udpEP.Write（不变，微秒级）
                              │ 远程转发:
                              │   1. cache.Get 命中 → 直接回包，返回
                              │   2. pendingQueries[domain] = {query, remoteAddr, at}
                              │   3. forwarderSocket.Write 发出
                              │   4. 返回（worker 立即处理下一个查询）
                              ▼
forwarderListener ◀── 收响应 ──▶ cache.Set
                              │ 查 pendingQueries[domain]
                              │   命中 → buildDNSResponse(p.query, ip) → udpEP.Write 回客户端
                              │   未命中 → 仅缓存（迟到响应，旧语义保留）
                              ▼
                       ticker(500ms) sweep: 注册超 5s 未响应
                              → 删 pending + buildDNSErrorResponse(SERVFAIL) 回客户端
```

### 关键决策

| 决策 | 理由 |
|---|---|
| pending value 存 `{query, remoteAddr, at}` 而非 channel | listener 直接回包无需中转；query 保留原始 TXID，`buildDNSResponse` 天然匹配 |
| 超时清扫放 listener 内 ticker，不 per-query 起 timer | 单 goroutine 单 ticker，qps 无关；旧行为"超时回 SERVFAIL 让客户端快重试"保留 |
| key 仍用 domain（小写归一），不按 txID | 同域名并发查询合并为一次转发（省上游流量），后注册者覆盖先注册者——旧实现同域并发同样只有一人能拿到响应，行为不退化 |
| cache key 统一 `strings.ToLower` | DNS 名字大小写不敏感，消除 resolver 大小写差异导致的缓存/ pending 失配（潜在 flap 源） |
| forwarderSocket==nil 时 Error 日志 + SERVFAIL，**删除 sync 兜底** | sync 路径（`forwardToRemoteSync`）用同一 netstack、绑同一 GIP，系统性失败下必然同样失败，救不了场还掩盖故障；失败要 loud，netstack 起不来的节点该修不该带病跑（用户裁决 2026-10-31） |
| worker 池维持 4 个 | 远程路径已零等待，池只为 CPU 解析服务，4 个富余 |

## 改动面

仅 `mesh/dns.go`：

- `pendingQueries` value 类型 `chan dnsResult` → `*pendingQuery`（新结构，含 query/remoteAddr/at）；删 `dnsResult`
- `forwardToRemote` → `forwardToRemoteAsync(subnet, query, remoteAddr)`：无返回值，worker 零等待；**删除** `forwardToRemoteSync` 兜底及 `gonet` 依赖
- `forwarderListener`：收包后查 pending 直接 `udpEP.Write`；循环加 ticker + `sweepPending`
- 新增 `sweepPending()`：超 5s 条目删 + SERVFAIL
- `processQuery` 转发分支改为调用 async 版
- `DNSCache.Get/Set` key 小写归一

## 验证

1. `go build ./...` + `go test ./mesh/...`
2. VM 部署（本机）后定向测试：
   - 连续 `Resolve-DnsName loc.qg.phn` ×50：无单条 >5s（除真实超时）
   - VM 上抓 `[DNS-FWD]` 日志确认 `START` 后 worker 不再出现 5s 停顿（日志时间戳间隔）
3. 部署 QG → jf（替换 10-30 dev 构建），观察 jf DNS 失声是否消失
