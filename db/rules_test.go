package db

import (
	"os"
	"testing"
)

func TestRuleStorage_JSONArray(t *testing.T) {
	// Create temporary database
	tmpFile := "/tmp/test-rules-" + t.Name() + ".db"
	defer os.Remove(tmpFile)

	if err := Init(tmpFile); err != nil {
		t.Fatalf("Failed to init database: %v", err)
	}
	defer Close()

	// Test 1: Empty database - insert first rule
	t.Run("InsertFirstRule", func(t *testing.T) {
		if err := InsertRule(0, "DOMAIN,google.com,Proxy"); err != nil {
			t.Fatalf("Failed to insert first rule: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if len(rules) != 1 {
			t.Fatalf("Expected 1 rule, got %d", len(rules))
		}
		if rules[0] != "DOMAIN,google.com,Proxy" {
			t.Fatalf("Expected 'DOMAIN,google.com,Proxy', got '%s'", rules[0])
		}
	})

	// Test 2: Insert at beginning
	t.Run("InsertAtBeginning", func(t *testing.T) {
		if err := InsertRule(0, "DOMAIN,facebook.com,Proxy"); err != nil {
			t.Fatalf("Failed to insert at beginning: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if len(rules) != 2 {
			t.Fatalf("Expected 2 rules, got %d", len(rules))
		}
		if rules[0] != "DOMAIN,facebook.com,Proxy" {
			t.Fatalf("Expected first rule 'DOMAIN,facebook.com,Proxy', got '%s'", rules[0])
		}
		if rules[1] != "DOMAIN,google.com,Proxy" {
			t.Fatalf("Expected second rule 'DOMAIN,google.com,Proxy', got '%s'", rules[1])
		}
	})

	// Test 3: Insert at end
	t.Run("InsertAtEnd", func(t *testing.T) {
		if err := InsertRule(2, "DOMAIN,twitter.com,Proxy"); err != nil {
			t.Fatalf("Failed to insert at end: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if len(rules) != 3 {
			t.Fatalf("Expected 3 rules, got %d", len(rules))
		}
		if rules[2] != "DOMAIN,twitter.com,Proxy" {
			t.Fatalf("Expected third rule 'DOMAIN,twitter.com,Proxy', got '%s'", rules[2])
		}
	})

	// Test 4: Insert in middle
	t.Run("InsertInMiddle", func(t *testing.T) {
		if err := InsertRule(1, "DOMAIN,youtube.com,Proxy"); err != nil {
			t.Fatalf("Failed to insert in middle: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if len(rules) != 4 {
			t.Fatalf("Expected 4 rules, got %d", len(rules))
		}
		expected := []string{
			"DOMAIN,facebook.com,Proxy",
			"DOMAIN,youtube.com,Proxy",
			"DOMAIN,google.com,Proxy",
			"DOMAIN,twitter.com,Proxy",
		}
		for i, exp := range expected {
			if rules[i] != exp {
				t.Fatalf("Rule %d: expected '%s', got '%s'", i, exp, rules[i])
			}
		}
	})

	// Test 5: Delete from middle
	t.Run("DeleteFromMiddle", func(t *testing.T) {
		if err := DeleteRuleByIndex(1); err != nil {
			t.Fatalf("Failed to delete from middle: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if len(rules) != 3 {
			t.Fatalf("Expected 3 rules, got %d", len(rules))
		}
		if rules[1] != "DOMAIN,google.com,Proxy" {
			t.Fatalf("Expected second rule 'DOMAIN,google.com,Proxy', got '%s'", rules[1])
		}
	})

	// Test 6: Update rule
	t.Run("UpdateRule", func(t *testing.T) {
		if err := PutRule(0, "DOMAIN,instagram.com,Proxy"); err != nil {
			t.Fatalf("Failed to update rule: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if rules[0] != "DOMAIN,instagram.com,Proxy" {
			t.Fatalf("Expected updated rule 'DOMAIN,instagram.com,Proxy', got '%s'", rules[0])
		}
	})

	// Test 7: Replace all rules
	t.Run("ReplaceAllRules", func(t *testing.T) {
		newRules := []string{
			"DOMAIN,a.com,Proxy",
			"DOMAIN,b.com,Proxy",
			"DOMAIN,c.com,Proxy",
		}
		if err := PutRules(newRules); err != nil {
			t.Fatalf("Failed to replace rules: %v", err)
		}

		rules, err := ListRules()
		if err != nil {
			t.Fatalf("Failed to list rules: %v", err)
		}
		if len(rules) != 3 {
			t.Fatalf("Expected 3 rules, got %d", len(rules))
		}
		for i, exp := range newRules {
			if rules[i] != exp {
				t.Fatalf("Rule %d: expected '%s', got '%s'", i, exp, rules[i])
			}
		}
	})

	// Test 8: Invalid index - insert out of bounds
	t.Run("InsertOutOfBounds", func(t *testing.T) {
		err := InsertRule(100, "DOMAIN,invalid.com,Proxy")
		if err == nil {
			t.Fatal("Expected error for out-of-bounds insert, got nil")
		}
	})

	// Test 9: Invalid index - delete out of bounds
	t.Run("DeleteOutOfBounds", func(t *testing.T) {
		err := DeleteRuleByIndex(100)
		if err == nil {
			t.Fatal("Expected error for out-of-bounds delete, got nil")
		}
	})

	// Test 10: Invalid index - negative
	t.Run("NegativeIndex", func(t *testing.T) {
		err := InsertRule(-1, "DOMAIN,invalid.com,Proxy")
		if err == nil {
			t.Fatal("Expected error for negative index, got nil")
		}
	})
}

func TestRuleStorage_EmptyDatabase(t *testing.T) {
	tmpFile := "/tmp/test-rules-empty-" + t.Name() + ".db"
	defer os.Remove(tmpFile)

	if err := Init(tmpFile); err != nil {
		t.Fatalf("Failed to init database: %v", err)
	}
	defer Close()

	// List rules from empty database
	rules, err := ListRules()
	if err != nil {
		t.Fatalf("Failed to list rules from empty database: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("Expected 0 rules, got %d", len(rules))
	}

	// Insert into empty database
	if err := InsertRule(0, "DOMAIN,test.com,Proxy"); err != nil {
		t.Fatalf("Failed to insert into empty database: %v", err)
	}

	rules, err = ListRules()
	if err != nil {
		t.Fatalf("Failed to list rules after insert: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("Expected 1 rule, got %d", len(rules))
	}
}
