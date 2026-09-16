# 清理旧版本遗留路由代码

## 背景

之前代码中包含大量 192.0.2.x 和 198.18.0.x 的清理逻辑，用于清理旧版本可能遗留的路由和邻居条目。实际上没有旧版本在线运行，这些代码是多余的。

## 变更

- `tun/route_windows.go`: 删除 setup 中的 192.0.2.x 邻居清理，删除 teardown 中的 192.0.2.x 路由清理
- `tun/cleanup_windows.go`: 简化 CleanupResidual，只保留当前版本的路由清理（on-link 0.0.0.0 next hop）
- `tun/route_api_windows.go`: deleteResidualRoutesAPI 只扫描 on-link 网关

## 状态

- [x] 完成
