package db

import (
	"encoding/json"
	"sort"
)

// Fake-IP 映射

// PutFakeIP 存储 Fake-IP 映射（domain 与 ip 双向索引）
func PutFakeIP(domain, ip string) error {
	entry := &FakeIPEntry{
		IP:     ip,
		Domain: domain,
	}

	if err := Put(BucketFakeIP, []byte("domain:"+domain), entry); err != nil {
		return err
	}

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

// DeleteFakeIP 删除 Fake-IP 映射（双向索引一起删）
func DeleteFakeIP(domain, ip string) error {
	if err := Delete(BucketFakeIP, []byte("domain:"+domain)); err != nil {
		return err
	}
	return Delete(BucketFakeIP, []byte("ip:"+ip))
}

// GetAllFakeIPs 获取所有 Fake-IP 映射
func GetAllFakeIPs() ([]*FakeIPEntry, error) {
	var entries []*FakeIPEntry

	err := ForEach(BucketFakeIP, func(k, v []byte) error {
		// Only process "domain:" keys to get complete entries
		key := string(k)
		if len(key) < 7 || key[:7] != "domain:" {
			return nil
		}

		var entry FakeIPEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		entries = append(entries, &entry)
		return nil
	})

	return entries, err
}

// 包元数据

// PutPackage 存储包元数据（key = ID）
func PutPackage(pkg *PackageMeta) error {
	return Put(BucketPackages, []byte(pkg.ID), pkg)
}

// GetPackage 获取包元数据（by ID）
func GetPackage(id string) (*PackageMeta, error) {
	var pkg PackageMeta
	err := Get(BucketPackages, []byte(id), &pkg)
	if err != nil {
		return nil, err
	}
	return &pkg, nil
}

// ListPackages 列出所有包（按上传时间倒序）
func ListPackages() ([]*PackageMeta, error) {
	var packages []*PackageMeta

	err := ForEach(BucketPackages, func(k, v []byte) error {
		var pkg PackageMeta
		if err := json.Unmarshal(v, &pkg); err != nil {
			return err
		}
		packages = append(packages, &pkg)
		return nil
	})

	if err != nil {
		return nil, err
	}

	// 按上传时间倒序排序
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].UploadedAt.After(packages[j].UploadedAt)
	})

	return packages, nil
}

// DeletePackage 删除包元数据（by ID）
func DeletePackage(id string) error {
	return Delete(BucketPackages, []byte(id))
}
