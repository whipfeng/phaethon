package db

import (
	"sync"
)

// OpType 操作类型
type OpType string

const (
	OpPut    OpType = "put"
	OpDelete OpType = "delete"
)

// ChangeEvent 数据变更事件
type ChangeEvent struct {
	Bucket string
	Key    string
	Op     OpType
}

// Watcher 数据变更回调
type Watcher func(event ChangeEvent)

type watcherEntry struct {
	bucket string // "" 表示监听所有 bucket
	fn     Watcher
}

var (
	watcherMu    sync.RWMutex
	watcherSeq   int64
	watcherTable = make(map[int64]*watcherEntry)
)

// Subscribe 订阅指定 bucket 的变更通知。bucket 传 nil 表示订阅所有 bucket。
// 返回 watchID，用于 Unsubscribe。
func Subscribe(bucket []byte, fn Watcher) int64 {
	name := ""
	if bucket != nil {
		name = string(bucket)
	}

	watcherMu.Lock()
	defer watcherMu.Unlock()

	watcherSeq++
	id := watcherSeq
	watcherTable[id] = &watcherEntry{bucket: name, fn: fn}
	return id
}

// Unsubscribe 取消订阅
func Unsubscribe(watchID int64) {
	watcherMu.Lock()
	defer watcherMu.Unlock()
	delete(watcherTable, watchID)
}

// notifyChanges 在写入成功后通知所有匹配的 watcher。
// 回调在写入方 goroutine 同步执行，必须快速返回，耗时工作请自行起 goroutine。
func notifyChanges(events []ChangeEvent) {
	if len(events) == 0 {
		return
	}

	watcherMu.RLock()
	entries := make([]*watcherEntry, 0, len(watcherTable))
	for _, e := range watcherTable {
		entries = append(entries, e)
	}
	watcherMu.RUnlock()

	for _, ev := range events {
		for _, entry := range entries {
			if entry.bucket != "" && entry.bucket != ev.Bucket {
				continue
			}
			safeCall(entry.fn, ev)
		}
	}
}

func safeCall(fn Watcher, ev ChangeEvent) {
	defer func() {
		_ = recover() // watcher 异常不影响数据库写入方
	}()
	fn(ev)
}
