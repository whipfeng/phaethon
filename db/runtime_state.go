package db

import (
	"encoding/json"
	"fmt"
	"time"
)

// ProxyHealth 代理健康状态
type ProxyHealth struct {
	GroupName string    `json:"group_name"`
	ProxyName string    `json:"proxy_name"`
	Alive     bool      `json:"alive"`
	Latency   int       `json:"latency"`    // 毫秒
	LastCheck time.Time `json:"last_check"`
	FailCount int       `json:"fail_count"`
}

// PutProxyHealth 存储代理健康状态
// Key格式: {groupName}:{proxyName}
func PutProxyHealth(groupName, proxyName string, health *ProxyHealth) error {
	if health == nil {
		return fmt.Errorf("health is nil")
	}
	health.GroupName = groupName
	health.ProxyName = proxyName
	key := fmt.Sprintf("%s:%s", groupName, proxyName)
	return Put(BucketProxyHealth, []byte(key), health)
}

// GetProxyHealth 获取单个代理的健康状态
func GetProxyHealth(groupName, proxyName string) (*ProxyHealth, error) {
	key := fmt.Sprintf("%s:%s", groupName, proxyName)
	return getView[ProxyHealth](BucketProxyHealth, []byte(key))
}

// ListProxyHealthByGroup 获取指定组的所有代理健康状态
func ListProxyHealthByGroup(groupName string) ([]*ProxyHealth, error) {
	prefix := groupName + ":"
	var results []*ProxyHealth
	
	err := ForEach(BucketProxyHealth, func(k, v []byte) error {
		key := string(k)
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			var health ProxyHealth
			if err := json.Unmarshal(v, &health); err != nil {
				return err
			}
			results = append(results, &health)
		}
		return nil
	})
	
	if err != nil {
		return nil, err
	}
	return results, nil
}

// DeleteProxyHealth 删除代理健康状态
func DeleteProxyHealth(groupName, proxyName string) error {
	key := fmt.Sprintf("%s:%s", groupName, proxyName)
	return Delete(BucketProxyHealth, []byte(key))
}

// ClearProxyHealthByGroup 清空指定组的所有健康状态
func ClearProxyHealthByGroup(groupName string) error {
	prefix := groupName + ":"
	var keysToDelete [][]byte
	
	err := ForEach(BucketProxyHealth, func(k, v []byte) error {
		key := string(k)
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
            keysToDelete = append(keysToDelete, k)
		}
		return nil
	})
	
	if err != nil {
		return err
	}
	
	for _, key := range keysToDelete {
		if err := Delete(BucketProxyHealth, key); err != nil {
			return err
		}
	}
	return nil
}

// SubscriptionNodes 订阅节点池
type SubscriptionNodes struct {
	SubscriptionName string      `json:"subscription_name"`
	Nodes            []NodeInfo  `json:"nodes"`
	LastUpdate       time.Time   `json:"last_update"`
}

// NodeInfo 节点信息
type NodeInfo struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Server string `json:"server"`
	Port   int    `json:"port"`
	// 其他代理字段...
}

// PutSubscriptionNodes 存储订阅节点池
func PutSubscriptionNodes(subName string, nodes *SubscriptionNodes) error {
	if nodes == nil {
		return fmt.Errorf("nodes is nil")
	}
	nodes.SubscriptionName = subName
	nodes.LastUpdate = time.Now()
	return Put(BucketSubscriptionNodes, []byte(subName), nodes)
}

// GetSubscriptionNodes 获取订阅节点池
func GetSubscriptionNodes(subName string) (*SubscriptionNodes, error) {
	return getView[SubscriptionNodes](BucketSubscriptionNodes, []byte(subName))
}

// DeleteSubscriptionNodes 删除订阅节点池
func DeleteSubscriptionNodes(subName string) error {
	return Delete(BucketSubscriptionNodes, []byte(subName))
}

// ListAllProxyHealth 列出所有代理健康状态
func ListAllProxyHealth() ([]*ProxyHealth, error) {
	return listView[ProxyHealth](BucketProxyHealth)
}

// ListAllSubscriptionNodes 列出所有订阅节点池
func ListAllSubscriptionNodes() ([]*SubscriptionNodes, error) {
	return listView[SubscriptionNodes](BucketSubscriptionNodes)
}
