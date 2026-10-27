package db

import "phaethon/util"

// skipBuckets 不需要触发版本通知的 bucket（运行时状态）
var skipBuckets = map[string]bool{
	string(BucketFakeIP):         true,
	string(BucketProxyHealth):    true,
	string(BucketRuntimeState):   true,
	string(BucketAuditLog):       true,
}

// InitVersionNotifier 初始化版本通知订阅
// 监听所有 bucket 变更，自动触发版本通知
// 在 main.go 中数据库初始化后调用
func InitVersionNotifier() {
	Subscribe(nil, func(event ChangeEvent) {
		if skipBuckets[event.Bucket] {
			return
		}
		// 直接用 bucket 名称作为 topic
		util.DefaultVersionNotifier.BumpVersion(event.Bucket)
	})
}
