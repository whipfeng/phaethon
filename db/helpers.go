package db

import (
	"encoding/json"
	"fmt"
)

// PutProxy 存储代理配置
func PutProxy(proxy *ProxyConfig) error {
	return Put(BucketProxies, []byte(proxy.Name), proxy)
}

// GetProxy 获取代理配置
func GetProxy(name string) (*ProxyConfig, error) {
	var proxy ProxyConfig
	err := Get(BucketProxies, []byte(name), &proxy)
	if err != nil {
		return nil, err
	}
	return &proxy, nil
}

// DeleteProxy 删除代理配置
func DeleteProxy(name string) error {
	return Delete(BucketProxies, []byte(name))
}

// ListProxies 列出所有代理
func ListProxies() ([]*ProxyConfig, error) {
	var proxies []*ProxyConfig
	
	err := ForEach(BucketProxies, func(k, v []byte) error {
		var proxy ProxyConfig
		if err := jsonUnmarshal(v, &proxy); err != nil {
			return err
		}
		proxies = append(proxies, &proxy)
		return nil
	})
	
	return proxies, err
}

// PutRule 存储规则配置
func PutRule(rule *RuleConfig) error {
	key := fmt.Sprintf("%06d", rule.Index)
	return Put(BucketRules, []byte(key), rule)
}

// GetRule 获取规则配置
func GetRule(index int) (*RuleConfig, error) {
	key := fmt.Sprintf("%06d", index)
	var rule RuleConfig
	err := Get(BucketRules, []byte(key), &rule)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// DeleteRule 删除规则配置
func DeleteRule(index int) error {
	key := fmt.Sprintf("%06d", index)
	return Delete(BucketRules, []byte(key))
}

// ListRules 列出所有规则（按索引排序）
func ListRules() ([]*RuleConfig, error) {
	var rules []*RuleConfig
	
	err := ForEach(BucketRules, func(k, v []byte) error {
		var rule RuleConfig
		if err := jsonUnmarshal(v, &rule); err != nil {
			return err
		}
		rules = append(rules, &rule)
		return nil
	})
	
	return rules, err
}

// PutMeshConfig 存储 Mesh 配置
func PutMeshConfig(config *MeshConfig) error {
	return Put(BucketConfig, []byte("mesh"), config)
}

// GetMeshConfig 获取 Mesh 配置
func GetMeshConfig() (*MeshConfig, error) {
	var config MeshConfig
	err := Get(BucketConfig, []byte("mesh"), &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// PutAdminConfig 存储 Admin 配置
func PutAdminConfig(config *AdminConfig) error {
	return Put(BucketConfig, []byte("admin"), config)
}

// GetAdminConfig 获取 Admin 配置
func GetAdminConfig() (*AdminConfig, error) {
	var config AdminConfig
	err := Get(BucketConfig, []byte("admin"), &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// PutFakeIP 存储 Fake-IP 映射
func PutFakeIP(domain, ip string) error {
	entry := &FakeIPEntry{
		IP:     ip,
		Domain: domain,
	}
	
	// 存储 domain -> entry
	err := Put(BucketFakeIP, []byte("domain:"+domain), entry)
	if err != nil {
		return err
	}
	
	// 存储 ip -> domain（反向查询）
	return Put(BucketFakeIP, []byte("ip:"+ip), map[string]string{"domain": domain})
}

// GetFakeIPByDomain 通过域名获取 Fake-IP
func GetFakeIPByDomain(domain string) (*FakeIPEntry, error) {
	var entry FakeIPEntry
	err := Get(BucketFakeIP, []byte("domain:"+domain), &entry)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// GetDomainByFakeIP 通过 Fake-IP 获取域名
func GetDomainByFakeIP(ip string) (string, error) {
	var result map[string]string
	err := Get(BucketFakeIP, []byte("ip:"+ip), &result)
	if err != nil {
		return "", err
	}
	return result["domain"], nil
}

// PutPackage 存储包元数据
func PutPackage(pkg *PackageMeta) error {
	key := fmt.Sprintf("%s:%s:%s", pkg.Platform, pkg.Arch, pkg.Version)
	return Put(BucketPackages, []byte(key), pkg)
}

// GetPackage 获取包元数据
func GetPackage(platform, arch, version string) (*PackageMeta, error) {
	key := fmt.Sprintf("%s:%s:%s", platform, arch, version)
	var pkg PackageMeta
	err := Get(BucketPackages, []byte(key), &pkg)
	if err != nil {
		return nil, err
	}
	return &pkg, nil
}

// ListPackages 列出所有包
func ListPackages() ([]*PackageMeta, error) {
	var packages []*PackageMeta
	
	err := ForEach(BucketPackages, func(k, v []byte) error {
		var pkg PackageMeta
		if err := jsonUnmarshal(v, &pkg); err != nil {
			return err
		}
		packages = append(packages, &pkg)
		return nil
	})
	
	return packages, err
}

// PutPeer 存储 Peer 信息
func PutPeer(peer *PeerInfo) error {
	return Put(BucketPeers, []byte(peer.NodeID), peer)
}

// GetPeer 获取 Peer 信息
func GetPeer(nodeID string) (*PeerInfo, error) {
	var peer PeerInfo
	err := Get(BucketPeers, []byte(nodeID), &peer)
	if err != nil {
		return nil, err
	}
	return &peer, nil
}

// ListPeers 列出所有 Peer
func ListPeers() ([]*PeerInfo, error) {
	var peers []*PeerInfo
	
	err := ForEach(BucketPeers, func(k, v []byte) error {
		var peer PeerInfo
		if err := jsonUnmarshal(v, &peer); err != nil {
			return err
		}
		peers = append(peers, &peer)
		return nil
	})
	
	return peers, err
}

// PutConnectionLog 存储连接日志
func PutConnectionLog(log *ConnectionLog) error {
	key := fmt.Sprintf("%d:%s", log.Timestamp.UnixNano(), log.ID)
	return Put(BucketLogs, []byte(key), log)
}

// ListConnectionLogs 列出连接日志
func ListConnectionLogs() ([]*ConnectionLog, error) {
	var logs []*ConnectionLog
	
	err := ForEach(BucketLogs, func(k, v []byte) error {
		var log ConnectionLog
		if err := jsonUnmarshal(v, &log); err != nil {
			return err
		}
		logs = append(logs, &log)
		return nil
	})
	
	return logs, err
}

// jsonUnmarshal 辅助函数
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
