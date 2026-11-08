# L1/L2 goroutine 泄漏处置（L1 修复 + L2 维持审计结论）

| | |
|---|---|
| 版本 | v1.0.0 |
| 状态 | **已结案**（2026-11-08 核实）：L1 已修复并有两项测试；L2 按 timeout-audit A2 决策维持不修 |
| 关联 | [mesh-data-plane-stall.md](../issues/mesh-data-plane-stall.md)（dump 130348 节 L1/L2 签名复核）、[timeout-audit-20261103.md](timeout-audit-20261103.md)（A2 既定决策） |

## 一、背景

13:03 dump（goroutine-dump-20261105-130348）复核出两条慢性泄漏签名：

- **L1**：`admin.(*singleConnListener).Accept` @ admin.go:907 `<-l.doneCh`，8 个、匀速 ~0.37/min。创建路径 `acceptTCP.func1.1 → HandleTCPConn → ServeConn → http.Server.Serve`。每来一条经 mesh 的 admin 连接漏一个 goroutine（Serve 在送走唯一连接后回到 Accept 永远阻塞），外推 24h 数百个。
- **L2**：`handler.relayUDP` wg.Wait @ connection_handler.go:615，3 组，双向读零流量无超时环形等待。

## 二、L1 根因与修复方案

**根因**：`ServeConn`（admin.go:875）创建 `singleConnListener` 后调 `server.Serve(ln)`，但**没有任何路径关闭 ln**。http.Server 服务完唯一连接后会 close 该连接，随后 Serve 循环回到 `Accept` 阻塞在 `<-l.doneCh` 永不返回 → `ServeConn` → `HandleTCPConn` → `acceptTCP.func1.1` 整条 goroutine 链泄漏。连接死得越快漏得越快（与观测速率吻合）。

注意 `defer ln.Close()` 单独无效：Serve 阻塞在 Accept 内部，defer 要等 Serve 返回才执行，构成循环等待。必须让**连接关闭事件级联到 listener 关闭**。

**方案**（只碰 `admin/admin.go`，加一个小包装类型）：

1. 新增 `closeCascadeConn`：包装原始 `net.Conn`，`Close()` 时先关底层、再幂等触发 `ln.Close()`。
2. `ServeConn` 改造：
   ```go
   ln := newSingleConnListener(nil)
   cascade := &closeCascadeConn{Conn: conn, onClose: ln.Close}
   if s.tlsConfig != nil {
       handledConn = tls.Server(cascade, s.tlsConfig)   // tls.Conn.Close 会级联到底层
   } else {
       handledConn = cascade
   }
   ln.conn = handledConn
   defer ln.Close() // 兜底（Serve 异常返回路径）
   ```
   TLS 情形：`tls.Conn.Close()` → `cascade.Close()` → `ln.Close()` → Accept 返回 io.EOF → Serve 返回 → ServeConn 返回 → goroutine 链正常退出。
3. Hijack 连接（htmx 流式）：连接生命周期移交 handler，handler close 连接时同样级联，无特殊处理。

**风险**：低。行为变化仅是"admin 连接关闭后 ServeConn goroutine 及时退出"；HTTP 语义不变。两个调用点（connection_handler.go:183 mesh 入向 / :255 本机回环）共用 ServeConn，一处修复全覆盖。

## 三、L2 处置：不改代码（维持 timeout-audit 既定决策）

L2 的"修复"只剩"给 relayUDP 加空闲超时"一条路，而 **[timeout-audit-20261103.md](timeout-audit-20261103.md) A2 已明确处置：删除 UDP 空闲看门狗、接受滞留**（中转哲学：数据面不设空闲杀；滞留面=客户端整机崩溃未 close socket，低频低量，接受）。重新引入空闲超时 = 推翻已批决策，本方案不做。

维持结论的监控项：后续 dump 中 relayUDP 组数若与在途 UDP 流数量级持续脱钩（如无流量环境数小时仍线性增长），再单独提案（届时方案只能是配置化的极长超时，同样需用户拍板）。

## 四、验证门禁

1. `go build ./...` + `go vet ./admin/` 通过；新增单测：
   - `TestServeConnReturnsAfterConnClose`：net.Pipe 建连 → ServeConn goroutine → 对端 close → 断言 ServeConn 在 2s 内返回。
   - `TestSingleConnListenerAcceptUnblocksOnClose`：close 后 Accept 返回 io.EOF。
2. 本机部署后：
   - 经 mesh 连续发起 ≥10 条 admin HTTP 连接并正常关闭，抓 goroutine dump（或 `/debug` 端点），`singleConnListener.Accept` 残留数 ≤1。
   - admin 功能回归：页面/API 正常，htmx 往返正常。
   - 四路探针 watcher 无异常。
3. 提交：phaethon 主仓库，plan + 实现 + 测试一个 commit（或 plan 先行单独 commit，遵循既往习惯）。

## 五、非目标

- 不改 relayUDP / relay（L2 维持现状，见 §三）。
- 不碰 gVisor fork、不碰 netstack。
- 不做全局 goroutine 数监控告警（超出本次范围）。
