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

func TestImportLoadRuleConfRoundtrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	enabled := true
	disabled := false
	conf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{Name: "p1", Type: "trojan", Server: "1.1.1.1", Port: 443, Password: "pw", Sni: "sni.example.com"},
			{Name: "p2", Type: "socks5", Server: "2.2.2.2", Port: 1080, Enabled: &disabled},
		},
		ProxyGroups: []*config.ProxyGroup{
			{Name: "g1", Type: "select", ManualProxies: []string{"p1", "p2"}},
		},
		Subscriptions: []*config.Subscription{
			{Name: "sub1", URL: "https://example.com/sub", Enabled: &enabled},
		},
		Rules: []string{
			"DOMAIN-SUFFIX,google.com,p1",
			"IP-CIDR,10.0.0.0/8,DIRECT",
			"MATCH,g1",
		},
		Mappings: []*config.Mapping{
			{Name: "m1", Type: "socks5", Port: 1080},
		},
		Resolvers: []*config.Resolver{
			{Name: "r1", SrcHost: "a.example.com", DstHost: "8.8.8.8", DstPort: 53},
		},
		ReverseConfigs: []*config.ReverseConfig{
			{Name: "rv1", RegistryProxy: "p1", TargetAddress: "127.0.0.1:80", LastError: "transient", AssignedPort: 39008},
		},
		Admin:        &config.AdminConfig{Enabled: true, Addr: "0.0.0.0:39999"},
		Mesh:         &config.MeshConfig{NodeID: "node-x", Subnet: "100.1.0.0/16"},
		UDPPortRange: "30000-30100",
	}

	if err := ImportRuleConf(conf); err != nil {
		t.Fatalf("ImportRuleConf failed: %v", err)
	}

	got, err := LoadRuleConf()
	if err != nil {
		t.Fatalf("LoadRuleConf failed: %v", err)
	}

	if len(got.Proxies) != 2 {
		t.Errorf("proxies = %d, want 2", len(got.Proxies))
	}
	if got.Proxies[0].Sni != "sni.example.com" {
		t.Errorf("proxy sni = %q", got.Proxies[0].Sni)
	}
	if got.Proxies[1].IsEnabled() {
		t.Errorf("proxy p2 should be disabled")
	}
	if len(got.ProxyGroups) != 1 || len(got.ProxyGroups[0].ManualProxies) != 2 {
		t.Errorf("proxy group manual proxies mismatch: %+v", got.ProxyGroups)
	}
	if len(got.Subscriptions) != 1 || got.Subscriptions[0].URL != "https://example.com/sub" {
		t.Errorf("subscriptions mismatch: %+v", got.Subscriptions)
	}
	if len(got.Rules) != 3 || got.Rules[0] != "DOMAIN-SUFFIX,google.com,p1" || got.Rules[2] != "MATCH,g1" {
		t.Errorf("rules order/content mismatch: %v", got.Rules)
	}
	if len(got.Mappings) != 1 || got.Mappings[0].Port != 1080 {
		t.Errorf("mappings mismatch: %+v", got.Mappings)
	}
	if len(got.Resolvers) != 1 {
		t.Errorf("resolvers mismatch: %+v", got.Resolvers)
	}
	if len(got.ReverseConfigs) != 1 || got.ReverseConfigs[0].RegistryProxy != "p1" {
		t.Errorf("reverse configs mismatch: %+v", got.ReverseConfigs)
	}
	if got.ReverseConfigs[0].LastError != "" || got.ReverseConfigs[0].AssignedPort != 0 {
		t.Errorf("runtime fields should be cleared on import: %+v", got.ReverseConfigs[0])
	}
	if got.Admin == nil || got.Admin.Addr != "0.0.0.0:39999" {
		t.Errorf("admin mismatch: %+v", got.Admin)
	}
	if got.Mesh == nil || got.Mesh.NodeID != "node-x" {
		t.Errorf("mesh mismatch: %+v", got.Mesh)
	}
	if got.UDPPortRange != "30000-30100" {
		t.Errorf("udp port range = %q", got.UDPPortRange)
	}

	// Init() 派生索引应已建立
	if got.ProxyNames["p1"] == nil {
		t.Errorf("ProxyNames not built")
	}
	if got.GroupNames["g1"] == nil {
		t.Errorf("GroupNames not built")
	}
}

func TestImportRuleConfReplacesAll(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	// 第一份配置
	first := &config.RuleConfiguration{
		Proxies: []*config.Proxy{{Name: "a", Type: "socks5", Server: "1.1.1.1", Port: 1}},
		Rules:   []string{"MATCH,a"},
	}
	if err := ImportRuleConf(first); err != nil {
		t.Fatalf("first import failed: %v", err)
	}

	// 第二份配置（内容更少，验证旧数据被整体替换而不是追加）
	second := &config.RuleConfiguration{
		Proxies: []*config.Proxy{{Name: "b", Type: "socks5", Server: "2.2.2.2", Port: 2}},
	}
	if err := ImportRuleConf(second); err != nil {
		t.Fatalf("second import failed: %v", err)
	}

	got, err := LoadRuleConf()
	if err != nil {
		t.Fatalf("LoadRuleConf failed: %v", err)
	}
	if len(got.Proxies) != 1 || got.Proxies[0].Name != "b" {
		t.Errorf("old proxy should be replaced: %+v", got.Proxies)
	}
	if len(got.Rules) != 0 {
		t.Errorf("old rules should be replaced: %v", got.Rules)
	}

	// 状态 bucket 不受导入影响
	if err := PutFakeIP("keep.com", "100.0.0.9"); err != nil {
		t.Fatalf("PutFakeIP failed: %v", err)
	}
	if err := ImportRuleConf(&config.RuleConfiguration{}); err != nil {
		t.Fatalf("empty import failed: %v", err)
	}
	d, err := GetDomainByFakeIP("100.0.0.9")
	if err != nil || d != "keep.com" {
		t.Errorf("fakeip should survive import: d=%q err=%v", d, err)
	}
}

func TestImportRuleConfSyntheticEvent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	var mu sync.Mutex
	var events []ChangeEvent
	Subscribe(nil, func(ev ChangeEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	conf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{Name: "a", Type: "socks5", Server: "1.1.1.1", Port: 1},
			{Name: "b", Type: "socks5", Server: "2.2.2.2", Port: 2},
		},
		Rules: []string{"MATCH,a"},
	}
	if err := ImportRuleConf(conf); err != nil {
		t.Fatalf("ImportRuleConf failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// 无论写入了多少 key，批量导入只触发一次合成事件
	if len(events) != 1 {
		t.Errorf("events = %d, want 1 (synthetic only): %+v", len(events), events)
	}
	if len(events) == 1 {
		ev := events[0]
		if ev.Bucket != string(BucketConfig) || ev.Key != KeyRebuilt || ev.Op != OpPut {
			t.Errorf("synthetic event = %+v, want config/__rebuilt__/put", ev)
		}
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
		Proxies:       []string{"p", "sub-node"}, // 运行时扁平表
		SubCandidateCount: 7,
	}
	if err := PutProxyGroup(g); err != nil {
		t.Fatalf("PutProxyGroup failed: %v", err)
	}

	got, err := GetProxyGroup("g")
	if err != nil {
		t.Fatalf("GetProxyGroup failed: %v", err)
	}
	if len(got.Proxies) != 0 {
		t.Errorf("runtime Proxies should not be persisted: %v", got.Proxies)
	}
	if got.SubCandidateCount != 0 {
		t.Errorf("SubCandidateCount should not be persisted: %d", got.SubCandidateCount)
	}
	if len(got.ManualProxies) != 1 || got.ManualProxies[0] != "p" {
		t.Errorf("ManualProxies mismatch: %v", got.ManualProxies)
	}
}

func TestRulesOrderedByIndex(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	// 乱序写入
	pairs := []struct {
		idx  int
		rule string
	}{{9, "idx9"}, {1, "idx1"}, {15, "idx15"}, {0, "idx0"}}
	for _, p := range pairs {
		if err := PutRule(p.idx, p.rule); err != nil {
			t.Fatalf("PutRule(%d) failed: %v", p.idx, err)
		}
	}

	conf, err := LoadRuleConf()
	if err != nil {
		t.Fatalf("LoadRuleConf failed: %v", err)
	}
	want := []string{"idx0", "idx1", "idx9", "idx15"}
	if len(conf.Rules) != len(want) {
		t.Fatalf("rules = %v, want %v", conf.Rules, want)
	}
	for i, w := range want {
		if conf.Rules[i] != w {
			t.Errorf("rules[%d] = %q, want %q", i, conf.Rules[i], w)
		}
	}

	// 删除中间规则后重读
	if err := DeleteRule(1); err != nil {
		t.Fatalf("DeleteRule failed: %v", err)
	}
	conf, err = LoadRuleConf()
	if err != nil {
		t.Fatalf("LoadRuleConf failed: %v", err)
	}
	if len(conf.Rules) != 3 || conf.Rules[1] != "idx9" {
		t.Errorf("rules after delete = %v", conf.Rules)
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

	// fakeip 不算配置
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

func TestLoadRuleConfEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := Init(dbPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	conf, err := LoadRuleConf()
	if err != nil {
		t.Fatalf("LoadRuleConf on empty db failed: %v", err)
	}
	if conf.Admin != nil || conf.Mesh != nil || conf.TUN != nil {
		t.Errorf("scalar sections should be nil on empty db: %+v", conf)
	}
	if len(conf.Proxies) != 0 || len(conf.Rules) != 0 {
		t.Errorf("lists should be empty on empty db")
	}
	if conf.ProxyNames["DIRECT"] == nil {
		t.Errorf("Init should register DIRECT builtin")
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
