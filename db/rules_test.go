package db

import (
	"errors"
	"os"
	"testing"
)

func TestRuleStorage_IDBased(t *testing.T) {
	tmpFile := "/tmp/test-rules-" + t.Name() + ".db"
	defer os.Remove(tmpFile)

	if err := Init(tmpFile); err != nil {
		t.Fatalf("Failed to init database: %v", err)
	}
	defer Close()

	mustList := func() []string {
		rules, err := ListRules()
		if err != nil {
			t.Fatalf("ListRules failed: %v", err)
		}
		return rules
	}

	// 1. Append to empty database
	idGoogle, err := InsertRuleWithID("DOMAIN,google.com,Proxy", nil)
	if err != nil {
		t.Fatalf("insert first rule: %v", err)
	}
	if rules := mustList(); len(rules) != 1 || rules[0] != "DOMAIN,google.com,Proxy" {
		t.Fatalf("expected 1 rule, got %v", rules)
	}

	// 2. Insert at beginning (afterID=0)
	if _, err := InsertRuleWithID("DOMAIN,facebook.com,Proxy", uint64Ptr(0)); err != nil {
		t.Fatalf("insert at beginning: %v", err)
	}
	rules := mustList()
	if len(rules) != 2 || rules[0] != "DOMAIN,facebook.com,Proxy" || rules[1] != "DOMAIN,google.com,Proxy" {
		t.Fatalf("beginning insert order wrong: %v", rules)
	}

	// 3. Insert after a specific ID
	if _, err := InsertRuleWithID("DOMAIN,youtube.com,Proxy", &idGoogle); err != nil {
		t.Fatalf("insert after id: %v", err)
	}
	rules = mustList()
	if len(rules) != 3 || rules[2] != "DOMAIN,youtube.com,Proxy" {
		t.Fatalf("after-ID insert order wrong: %v", rules)
	}

	// 4. Update by ID
	if err := UpdateRuleByID(idGoogle, "DOMAIN,google.com,Direct"); err != nil {
		t.Fatalf("update by id: %v", err)
	}
	if rules := mustList(); rules[1] != "DOMAIN,google.com,Direct" {
		t.Fatalf("update not applied: %v", rules)
	}

	// 5. Delete by ID
	if err := DeleteRuleByID(idGoogle); err != nil {
		t.Fatalf("delete by id: %v", err)
	}
	rules = mustList()
	if len(rules) != 2 || rules[0] != "DOMAIN,facebook.com,Proxy" || rules[1] != "DOMAIN,youtube.com,Proxy" {
		t.Fatalf("delete order wrong: %v", rules)
	}

	// 6. Insert with nonexistent afterID errors
	if _, err := InsertRuleWithID("DOMAIN,x.com,Proxy", uint64Ptr(999)); err == nil {
		t.Fatal("expected error for nonexistent afterID")
	}

	// 7. Delete nonexistent ID errors
	if err := DeleteRuleByID(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRuleStorage_EmptyDatabase(t *testing.T) {
	tmpFile := "/tmp/test-rules-empty-" + t.Name() + ".db"
	defer os.Remove(tmpFile)

	if err := Init(tmpFile); err != nil {
		t.Fatalf("Failed to init database: %v", err)
	}
	defer Close()

	rules, err := ListRules()
	if err != nil {
		t.Fatalf("ListRules on empty db: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules, got %d", len(rules))
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }
