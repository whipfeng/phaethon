package db

import (
	"time"

	"phaethon/config"
)

// SubscriptionNodes 订阅节点池
type SubscriptionNodes struct {
	SubscriptionName string                `json:"subscription_name"`
	Proxies          map[string]*config.Proxy `json:"proxies"`
	LastUpdate       time.Time             `json:"last_update"`
}

// PutSubscriptionNodes 存储订阅节点池
func PutSubscriptionNodes(subName string, proxies map[string]*config.Proxy) error {
	nodes := &SubscriptionNodes{
		SubscriptionName: subName,
		Proxies:          proxies,
		LastUpdate:       time.Now(),
	}
	return Put(BucketSubscriptionNodes, []byte(subName), nodes)
}

// GetSubscriptionNodes 获取订阅节点池
func GetSubscriptionNodes(subName string) (map[string]*config.Proxy, error) {
	nodes, err := getView[SubscriptionNodes](BucketSubscriptionNodes, []byte(subName))
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		return nil, nil
	}
	return nodes.Proxies, nil
}

// DeleteSubscriptionNodes 删除订阅节点池
func DeleteSubscriptionNodes(subName string) error {
	return Delete(BucketSubscriptionNodes, []byte(subName))
}

// ListAllSubscriptionNodes 列出所有订阅节点池
func ListAllSubscriptionNodes() ([]*SubscriptionNodes, error) {
	return listView[SubscriptionNodes](BucketSubscriptionNodes)
}

// OnGroupHealthSaved is invoked when a group's health snapshot is refreshed.
// Injected by main (wired to the admin stats collector) so dashboard health
// views update event-driven. Health data is display-only and intentionally
// NOT persisted: after restart the dashboard shows no health until the first
// check round completes, which is honest (stale health is misleading anyway)
// and avoids a bbolt fsync per node per check round.
var OnGroupHealthSaved func(groupName string, snap map[string]config.HealthInfo)

// NotifyGroupHealth broadcasts a group's fresh health snapshot to in-memory
// consumers (replacing the old SaveGroupHealthState that persisted to the
// proxy-health bucket).
func NotifyGroupHealth(groupName string, snap map[string]config.HealthInfo) {
	if OnGroupHealthSaved != nil {
		OnGroupHealthSaved(groupName, snap)
	}
}
