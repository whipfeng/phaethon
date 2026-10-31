package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"phaethon/config"
)

func TestNotifyMechanism(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	var mu sync.Mutex
	var events []ChangeEvent

	// 订阅 proxies bucket
	watchID := Subscribe(BucketProxies, func(ev ChangeEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	defer Unsubscribe(watchID)

	// 订阅所有 bucket
	var allEvents []ChangeEvent
	watchAllID := Subscribe(nil, func(ev ChangeEvent) {
		mu.Lock()
		allEvents = append(allEvents, ev)
		mu.Unlock()
	})
	defer Unsubscribe(watchAllID)

	// 触发 Put
	proxy := &config.Proxy{Name: "test-proxy", Type: "socks5", Server: "1.2.3.4", Port: 1080}
	if err := PutProxy(proxy); err != nil {
		t.Fatalf("PutProxy failed: %v", err)
	}

	// 触发 Delete
	if err := DeleteProxy("test-proxy"); err != nil {
		t.Fatalf("DeleteProxy failed: %v", err)
	}

	// 触发一个其他 bucket 的写入（proxies watcher 不应收到）
	if err := PutFakeIP("example.com", "100.0.0.1"); err != nil {
		t.Fatalf("PutFakeIP failed: %v", err)
	}

	// 等待事件送达（同步调用，应该立即完成）
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// 验证 bucket watcher 收到 2 个事件（put + delete）
	if len(events) != 2 {
		t.Errorf("bucket watcher got %d events, want 2: %+v", len(events), events)
	}
	if len(events) >= 1 {
		if events[0].Op != OpPut || events[0].Key != "test-proxy" {
			t.Errorf("event[0] = %+v, want put/test-proxy", events[0])
		}
	}
	if len(events) >= 2 {
		if events[1].Op != OpDelete {
			t.Errorf("event[1] = %+v, want delete", events[1])
		}
	}

	// 验证 watch-all 收到 4 个事件
	// （2 个 proxies + fakeip 正反两条索引记录）
	if len(allEvents) != 4 {
		t.Errorf("watch-all got %d events, want 4: %+v", len(allEvents), allEvents)
	}
}

func TestUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	var count int
	var mu sync.Mutex

	watchID := Subscribe(BucketProxies, func(ev ChangeEvent) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	// 写一次 → 收到
	if err := Put(BucketProxies, []byte("k1"), "v1"); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// 取消订阅
	Unsubscribe(watchID)

	// 再写 → 不应收到
	if err := Put(BucketProxies, []byte("k2"), "v2"); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("count = %d, want 1 (after unsubscribe no more events)", count)
	}
}

func TestWatcherPanicIsolation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	// 注册一个会 panic 的 watcher
	Subscribe(BucketProxies, func(ev ChangeEvent) {
		panic("watcher panic")
	})

	var okCount int
	var mu sync.Mutex
	Subscribe(BucketProxies, func(ev ChangeEvent) {
		mu.Lock()
		okCount++
		mu.Unlock()
	})

	// panic 的 watcher 不应影响写入和另一个 watcher
	if err := Put(BucketProxies, []byte("k"), fmt.Sprintf("v")); err != nil {
		t.Fatalf("Put failed (panic isolation broken): %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if okCount != 1 {
		t.Errorf("okCount = %d, want 1", okCount)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// 第一次打开，写入数据
	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	proxy := &config.Proxy{Name: "persist-test", Type: "trojan", Server: "5.6.7.8", Port: 443}
	if err := PutProxy(proxy); err != nil {
		t.Fatalf("PutProxy failed: %v", err)
	}
	mesh := &config.MeshConfig{NodeID: "test-node-123"}
	if err := PutMeshConfig(mesh); err != nil {
		t.Fatalf("PutMeshConfig failed: %v", err)
	}
	if err := Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// 第二次打开，读取数据
	if err := Init(dbPath); err != nil {
		t.Fatalf("re-Init failed: %v", err)
	}
	defer Close()

	got, err := GetProxy("persist-test")
	if err != nil {
		t.Fatalf("GetProxy failed: %v", err)
	}
	if got.Server != "5.6.7.8" || got.Port != 443 {
		t.Errorf("proxy mismatch: %+v", got)
	}

	gotMesh, err := GetMeshConfig()
	if err != nil {
		t.Fatalf("GetMeshConfig failed: %v", err)
	}
	if gotMesh.NodeID != "test-node-123" {
		t.Errorf("mesh nodeID = %q, want %q", gotMesh.NodeID, "test-node-123")
	}
}

func TestGroupRuntimeFieldsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	g := &config.ProxyGroup{
		Name:          "g",
		Type:          "select",
		ManualProxies: []string{"p"},
	}
	if err := PutProxyGroup(g); err != nil {
		t.Fatalf("PutProxyGroup failed: %v", err)
	}

	got, err := GetProxyGroup("g")
	if err != nil {
		t.Fatalf("GetProxyGroup failed: %v", err)
	}
	// storeGroup persists ManualProxies as-is; runtime-derived Proxies is not stored.
	if len(got.ManualProxies) != 1 || got.ManualProxies[0] != "p" {
		t.Errorf("ManualProxies should be persisted as-is: %v", got.ManualProxies)
	}
}

func TestRulesOrderedByIndex(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	// 通过 afterID 链按非插入顺序构建最终顺序 idx0,idx1,idx9,idx15
	id0, err := InsertRuleWithID("idx0", nil)
	if err != nil {
		t.Fatalf("insert idx0: %v", err)
	}
	id9, err := InsertRuleWithID("idx9", &id0)
	if err != nil {
		t.Fatalf("insert idx9: %v", err)
	}
	if _, err := InsertRuleWithID("idx15", &id9); err != nil {
		t.Fatalf("insert idx15: %v", err)
	}
	// 在 idx0 之后插入 idx1，使最终顺序为 idx0,idx1,idx9,idx15
	id1, err := InsertRuleWithID("idx1", &id0)
	if err != nil {
		t.Fatalf("insert idx1: %v", err)
	}

	rules, err := ListRules()
	if err != nil {
		t.Fatalf("ListRules failed: %v", err)
	}
	want := []string{"idx0", "idx1", "idx9", "idx15"}
	if len(rules) != len(want) {
		t.Fatalf("rules = %v, want %v", rules, want)
	}
	for i, w := range want {
		if rules[i] != w {
			t.Errorf("rules[%d] = %q, want %q", i, rules[i], w)
		}
	}

	// 删除中间规则后重读
	if err := DeleteRuleByID(id1); err != nil {
		t.Fatalf("DeleteRuleByID failed: %v", err)
	}
	rules, err = ListRules()
	if err != nil {
		t.Fatalf("ListRules failed: %v", err)
	}
	if len(rules) != 3 || rules[1] != "idx9" {
		t.Errorf("rules after delete = %v", rules)
	}
}

func TestIsEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	empty, err := IsEmptyConfig()
	if err != nil {
		t.Fatalf("IsEmptyConfig failed: %v", err)
	}
	if !empty {
		t.Errorf("fresh db should be empty")
	}

	if err := PutProxy(&config.Proxy{Name: "x", Type: "socks5"}); err != nil {
		t.Fatalf("PutProxy failed: %v", err)
	}
	empty, err = IsEmptyConfig()
	if err != nil {
		t.Fatalf("IsEmptyConfig failed: %v", err)
	}
	if empty {
		t.Errorf("db with proxy should not be empty")
	}

	// fakeip 不算配置（db 包是全局单句柄，重开新库前先 Close）
	Close()
	if err := Init(filepath.Join(dir, "test2.db")); err != nil {
		t.Fatalf("Init test2 failed: %v", err)
	}
	defer Close()
	if err := PutFakeIP("a.com", "100.0.0.1"); err != nil {
		t.Fatalf("PutFakeIP failed: %v", err)
	}
	empty, err = IsEmptyConfig()
	if err != nil {
		t.Fatalf("IsEmptyConfig failed: %v", err)
	}
	if !empty {
		t.Errorf("state-only db should still report empty config")
	}
}

func TestGetNotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	_, err := GetProxy("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMain(m *testing.M) {
	// 测试前确保没有残留的全局状态
	os.Exit(m.Run())
}
