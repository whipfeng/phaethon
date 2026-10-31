package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"phaethon/db"

	bolt "go.etcd.io/bbolt"
)

// rulesOldKey 是旧格式的单一 key（JSON 数组），主应用已不携带此格式，
// 由本工具自给自足。
var rulesOldKey = []byte("__all_rules__")

func listRulesOld() ([]string, error) {
	var rules []string
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(db.BucketRules)
		if b == nil {
			return nil
		}
		data := b.Get(rulesOldKey)
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &rules)
	})
	return rules, err
}

func deleteOldRulesKey() error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(db.BucketRules)
		if b == nil {
			return nil
		}
		return b.Delete(rulesOldKey)
	})
}

func main() {
	dryRun := flag.Bool("dry-run", false, "只检查不迁移")
	cleanup := flag.Bool("cleanup", false, "清理旧格式数据（迁移成功后调用）")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <db-path>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	dbPath := flag.Arg(0)

	fmt.Println("=== 规则迁移工具（旧格式 → 新格式）===")
	fmt.Println()

	// 初始化数据库
	if err := db.Init(dbPath); err != nil {
		fmt.Printf("❌ 初始化数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// 1. 读取旧格式
	oldRules, err := listRulesOld()
	if err != nil {
		fmt.Printf("❌ 读取旧格式失败: %v\n", err)
		os.Exit(1)
	}

	if len(oldRules) == 0 {
		fmt.Println("✓ 旧格式规则为空，无需迁移")
		return
	}

	fmt.Printf("📖 读取到 %d 条旧格式规则\n", len(oldRules))

	if *dryRun {
		fmt.Println("\n[DRY RUN] 将迁移以下规则:")
		for i, rule := range oldRules {
			fmt.Printf("  %d: %s\n", i+1, rule)
		}
		fmt.Println("\n✓ Dry run 完成，未实际迁移")
		return
	}

	// 2. 检查是否已经迁移过
	newRules, err := db.ListRulesWithIDs()
	if err != nil {
		fmt.Printf("❌ 读取新格式失败: %v\n", err)
		os.Exit(1)
	}
	if len(newRules) > 0 {
		fmt.Printf("⚠️  新格式已有 %d 条规则，可能已经迁移过\n", len(newRules))
		fmt.Println("如需重新迁移，请先清理数据库")
		os.Exit(1)
	}

	// 3. 迁移到新格式
	fmt.Println("\n🔄 开始迁移...")
	for i, rule := range oldRules {
		// 插入规则（追加到末尾）
		id, err := db.InsertRuleWithID(rule, nil)
		if err != nil {
			fmt.Printf("❌ 迁移规则失败 (index=%d): %v\n", i, err)
			os.Exit(1)
		}
		fmt.Printf("  ✓ 迁移规则 ID=%d: %s\n", id, truncate(rule, 50))
	}

	// 4. 验证迁移结果
	fmt.Println("\n🔍 验证迁移结果...")
	migratedRules, err := db.ListRulesWithIDs()
	if err != nil {
		fmt.Printf("❌ 验证失败: %v\n", err)
		os.Exit(1)
	}

	if len(migratedRules) != len(oldRules) {
		fmt.Printf("❌ 数量不一致: 旧=%d, 新=%d\n", len(oldRules), len(migratedRules))
		os.Exit(1)
	}

	fmt.Printf("✓ 成功迁移 %d 条规则\n", len(migratedRules))

	// 5. 清理旧格式（可选）
	if *cleanup {
		fmt.Println("\n🧹 清理旧格式数据...")
		if err := deleteOldRulesKey(); err != nil {
			fmt.Printf("⚠️  清理旧格式失败: %v\n", err)
			fmt.Println("可以稍后手动清理")
		} else {
			fmt.Println("✓ 旧格式数据已清理")
		}
	}

	fmt.Println("\n✅ 迁移完成！")
	fmt.Println("\n提示:")
	fmt.Println("  - 如果迁移成功，可以运行: ./migrate_rules --cleanup 清理旧数据")
	fmt.Println("  - 如果迁移失败，请恢复数据库备份")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
