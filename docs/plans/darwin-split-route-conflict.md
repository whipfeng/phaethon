# darwin 0/1 异主冲突抢占 + 真默认路由检测 设计

> 背景：docs/tasks/darwin-forwarder-fixes-tasks.md 遗留项。phase_d 实锤两个关联缺陷：
> ① holder/别家软件持有 0/1 时实例 enable `route add 0/1: file exists` 直接失败；
> ② `defaultRoute()` 的 RTM_GET(0.0.0.0/0) 走最长前缀匹配，0/1 存在时命中 0/1
> （phase_d 实测返回 gateway=198.18.0.1 iface=utun7，而非真默认 192.168.1.101/en0）。
> 用户裁决：按最合理方式修，不设工作量上限。

## 方案

### 核心思路：先 preempt 异主 split，再做 defaultRoute

platformSetup 顺序调整为：

```
0.   ifconfig 配地址（得 tunIndex）
0.5  preemptForeignSplits：探测 64.0.0.1 / 192.0.0.1
     - RTM_GET 最长前缀命中 /1（0.0.0.0/1 或 128.0.0.0/1）且 iface ≠ 本 tun
       → 快照（dst+mask、形态[经网关 IP | 经接口]、接口 index）→ 删除
     - 命中的是 /0 默认或其他非 /1 → 无冲突，不动
     - 探测/删除失败 → warn 继续（保持现状：后续 add EEXIST 会报错退出）
1.   defaultRoute()（此刻 0/1 已清，RTM_GET 必中真默认）
2.   排除路由（经原网关）
3.   加 0/1+128/1（经本 tun，此时绝无 EEXIST）
4.   ifscope 物理默认
```

teardown / setup 失败路径：`restoreForeignSplits()` —— 先删自己的 0/1+128/1 与
ifscope，再逐条恢复快照；**仅当快照接口 index 仍可解析**（holder 还活着）才恢复——
自家 kill -9 残留指向已销毁 utun 的快照自然跳过，不会留垃圾路由。

### 快照结构（定义在 route.go，纯数据，跨平台零影响）

```go
splitRouteSnapshot{ dst *net.IPNet, gwIP net.IP, gwIndex int }
// gwIP != nil → 经网关形态（routeAddNetViaGateway 恢复）
// gwIP == nil → 经接口形态（routeAddNetViaInterface(gwIndex) 恢复）
```

### routeProbeSplit(dst)（tun/routesock_darwin.go 新增）

RTM_GET(dst/32, rtaDST)，解析回包 sockaddrs（RTA_DST/RTA_GATEWAY/RTA_NETMASK）：
- netmask == /1 → 返回快照所需字段 + rtm_index（回包自带，无需 net.InterfaceByName）
- 其余 → (nil, nil) 表示无 split 冲突

**实证修正（phase_d 调试）**：xnu 对 RTM_GET 回包的 netmask 用截断编码——
/1 不是 16 字节 AF_INET sockaddr，而是 `05 ff ff ff 80`（family=0xff 标记 +
仅非零字节，/1 即首字节 0x80）。按 AF_INET 解析会得 nil 误判"无冲突"，
preempt 静默失效。`sockaddrNetmask` 须同时接受截断形式（offset 4 起为
有效前缀，右侧补零）与完整形式。

### 与已知形态的关系

- 自家 kill -9 残留 0/1（经死 utun）→ preempt 当异主删掉、teardown 时接口不存在跳过恢复 ✓
- holder（活进程，0/1 via 198.18.0.1 经网关形态）→ preempt 删、disable 时恢复 ✓（这是本次修复的目标场景）
- 别家 split-tunnel VPN（经网关形态）→ 同理抢占+恢复 ✓
- 无冲突常态 → 探测命中 /0，零行为变化 ✓

## 改动清单（darwin 限定 + route.go 纯数据字段）

1. tun/routesock_darwin.go：`routeProbeSplit(dst net.IP) (*splitRouteSnapshot, bool, error)`
2. tun/route.go：RouteManager 加 `foreignSplits []splitRouteSnapshot` + `tookForeignSplits bool`
3. tun/route_darwin.go：platformSetup 插 preempt（step 0.5）；失败路径恢复；platformTeardown 尾部恢复
4. tests/e2e/tun/mac_verify/phase_d.sh：加 disable 后异主 0/1 恢复断言

## 验证

- 三平台 go build
- Mac phase_d：holder 0/1 存在时实例 TUN running:true；holder 地址/路由全程完好；disable 后 holder 的 0/1 **被恢复**（netstat 复核）
- 回归 phase_a / phase_b / bindprobe 矩阵 / 运行时日志零错误 / CPU 0%
- kill -9 残留场景：残留 0/1 被 preempt 清掉且不做无谓恢复
