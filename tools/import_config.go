package main

import (
	"fmt"
	"os"

	"phaethon/config"
	"phaethon/db"
	"phaethon/util"
	"gopkg.in/yaml.v3"
)

type OldConfig struct {
	Proxies []*config.Proxy `yaml:"proxies"`
	Rules   []string        `yaml:"rules"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: import-config <config.yaml>")
		os.Exit(1)
	}

	configFile := os.Args[1]

	// Initialize database
	if err := db.Init("data/phaethon.db"); err != nil {
		util.LogError("Failed to init database: %v", err)
		os.Exit(1)
	}
	defer db.Close()

	// Read config file
	data, err := os.ReadFile(configFile)
	if err != nil {
		util.LogError("Failed to read config file: %v", err)
		os.Exit(1)
	}

	var oldCfg OldConfig
	if err := yaml.Unmarshal(data, &oldCfg); err != nil {
		util.LogError("Failed to parse config: %v", err)
		os.Exit(1)
	}

	util.LogInfo("Found %d proxies and %d rules", len(oldCfg.Proxies), len(oldCfg.Rules))

	// Import proxies
	for _, proxy := range oldCfg.Proxies {
		if err := db.PutProxy(proxy); err != nil {
			util.LogError("Failed to import proxy %s: %v", proxy.Name, err)
		} else {
			util.LogInfo("✅ Imported proxy: %s", proxy.Name)
		}
	}

	// Import rules
	for i, rule := range oldCfg.Rules {
		if _, err := db.InsertRuleWithID(rule, nil); err != nil {
			util.LogError("Failed to import rule %d: %v", i, err)
		} else {
			util.LogInfo("✅ Imported rule %d: %s", i, rule)
		}
	}

	util.LogInfo("Import completed!")
}
