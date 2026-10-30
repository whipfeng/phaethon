package db

import (
	"time"

	"phaethon/util"
)

// skipBuckets 不需要触发版本通知的 bucket（运行时状态）
var skipBuckets = map[string]bool{
	string(BucketFakeIP):         true,
	string(BucketRuntimeState):   true,
	string(BucketAuditLog):       true,
	string(BucketVersions):       true, // 版本号 bucket 本身不触发通知
}

// InitVersionNotifier 初始化版本通知订阅
// 在 main.go 中数据库初始化后调用
func InitVersionNotifier() {
	// 1. 用数据库回调初始化 VersionNotifier
	util.DefaultVersionNotifier = util.NewVersionNotifier(func() map[string]interface{} {
		versions := readAllVersions()
		result := make(map[string]interface{}, len(versions))
		for k, v := range versions {
			result[k] = v
		}
		return result
	})

	// 2. 递增 boot 版本号（标识服务器启动）
	bootVersion, _ := incrementVersion("boot")
	util.LogModuleInfo("version-notifier", "Server boot version: %d", bootVersion)

	// 3. 订阅 versions bucket 变化，自动通知 SSE 客户端
	Subscribe(BucketVersions, func(event ChangeEvent) {
		topic := string(event.Key)
		version := readVersion(topic)
		
		// 发送增量更新（只发送变化的 topic）
		util.DefaultVersionNotifier.BumpVersion(topic, version)
	})

	// 4. 订阅其他 bucket 变化，自动递增对应版本号
	Subscribe(nil, func(event ChangeEvent) {
		if skipBuckets[event.Bucket] {
			return
		}
		// 递增该 bucket 对应的版本号
		topic := event.Bucket
		newVersion, err := incrementVersion(topic)
		if err != nil {
			util.LogModuleError("version-notifier", "Failed to increment version for %s: %v", topic, err)
			return
		}
		// 发送增量更新
		util.DefaultVersionNotifier.BumpVersion(topic, newVersion)
	})

	// 5. 启动定期心跳（全量同步，10秒间隔）
	util.DefaultVersionNotifier.StartHeartbeat(10 * time.Second)
}

// BumpRuntimeVersion 手动递增运行时状态的版本号
// 用于 mesh、logs、tun、p2p 等不直接对应 bucket 的运行时状态
func BumpRuntimeVersion(topic string) {
	newVersion, err := incrementVersion(topic)
	if err != nil {
		util.LogModuleError("version-notifier", "Failed to increment runtime version for %s: %v", topic, err)
		return
	}
	util.DefaultVersionNotifier.BumpVersion(topic, newVersion)
}
