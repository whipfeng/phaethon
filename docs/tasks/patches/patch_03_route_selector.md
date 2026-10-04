# gVisor Fork 补丁 #3: RouteSelector 扩展点

## 目标

在 FindRoute 过程中注入自定义路由决策逻辑，处理路由表无法表达的复杂场景（如 fakeIP 本地交付、IPIP 封装决策）。

## 修改文件

1. `pkg/tcpip/stack/stack.go` - 添加 RouteSelector 类型和字段
2. `pkg/tcpip/stack/stack.go` - 修改 FindRoute 调用 RouteSelector

## 实现步骤

### 步骤 1：定义类型（stack.go 开头，Stack struct 之前）

```go
// RouteDecision represents the decision made by RouteSelector.
type RouteDecision struct {
    // NeedIPIP indicates whether the packet needs IPIP encapsulation.
    NeedIPIP bool
    
    // EgressVIP is the VIP of the egress node for IPIP encapsulation.
    // Only valid when NeedIPIP is true.
    EgressVIP tcpip.Address
    
    // Cacheable indicates whether this decision can be cached.
    // Set to false for dynamic routing decisions (e.g., load-based).
    Cacheable bool
    
    // LocalDelivery indicates the packet should be delivered locally
    // (e.g., to Forwarder for fakeIP destinations).
    LocalDelivery bool
}

// RouteSelector is a callback function that makes routing decisions
// during FindRoute. It can override normal route table lookup for
// special cases like fakeIP, IPIP encapsulation, etc.
type RouteSelector func(dst tcpip.Address) RouteDecision
```

### 步骤 2：添加字段到 Stack struct（stack.go:75-179）

在 Stack struct 中添加：

```go
type Stack struct {
    // ... 现有字段 ...
    
    // routeSelector is an optional callback for custom routing decisions.
    // If set, it's called during FindRoute to handle special cases.
    routeSelector RouteSelector `state:"nosave"`
}
```

### 步骤 3：添加设置方法（stack.go 末尾）

```go
// SetRouteSelector sets the route selector callback.
// The callback is invoked during FindRoute to make custom routing decisions.
func (s *Stack) SetRouteSelector(rs RouteSelector) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.routeSelector = rs
}
```

### 步骤 4：修改 FindRoute（stack.go FindRoute 函数）

在 FindRoute 函数的路由表查询之前，调用 RouteSelector：

```go
func (s *Stack) FindRoute(id tcpip.NICID, localAddr, remoteAddr tcpip.Address, netProto tcpip.NetworkProtocolNumber, multicastLoop bool) (*Route, tcpip.Error) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    // ... 现有检查 ...
    
    // Phaethon patch #3: Call RouteSelector if set
    if s.routeSelector != nil {
        decision := s.routeSelector(remoteAddr)
        
        if decision.LocalDelivery {
            // Create a local delivery route
            // ... 实现本地交付路由 ...
        }
        
        if decision.NeedIPIP {
            // Create an IPIP encapsulation route
            // ... 实现 IPIP 路由 ...
        }
    }
    
    // ... 继续现有路由表查询逻辑 ...
}
```

## 验收标准

- [ ] RouteSelector 类型定义正确
- [ ] Stack 包含 routeSelector 字段
- [ ] SetRouteSelector 方法可用
- [ ] FindRoute 调用 RouteSelector
- [ ] 编译通过：`cd ../gvisor-fork && go build ./...`

## 注意事项

1. RouteSelector 在 mu.RLock 保护下调用，不能修改 Stack 状态
2. RouteDecision 的字段需要根据实际实现填充
3. LocalDelivery 和 NeedIPIP 的具体路由创建逻辑需要后续补丁完善
