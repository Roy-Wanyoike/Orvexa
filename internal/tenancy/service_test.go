package tenancy

import (
	"testing"
)

func TestHashKeyIsDeterministicAndNonReversible(t *testing.T) {
	h1 := HashKey("orvx_secret_key_123")
	h2 := HashKey("orvx_secret_key_123")
	if h1 != h2 {
		t.Fatal("hash must be deterministic")
	}
	if h1 == "orvx_secret_key_123" || len(h1) != 64 {
		t.Fatal("hash must be a 64-char hex digest, never the raw key")
	}
}

func TestNewRawKeyFormat(t *testing.T) {
	k := NewRawKey()
	if len(k) != len("orvx_")+48 {
		t.Fatalf("key length wrong: %d", len(k))
	}
	if k[:5] != "orvx_" {
		t.Fatal("prefix missing")
	}
	if HashKey(k) == k {
		t.Fatal("hash collision with raw key")
	}
}

func TestPrincipalScopes(t *testing.T) {
	p := &Principal{Scopes: []string{"interactions:read", "cases:write"}}
	if !p.HasScope("interactions:read") {
		t.Error("listed scope must match")
	}
	if p.HasScope("admin:*") {
		t.Error("unlisted scope must not match")
	}
	if !(&Principal{Scopes: []string{"*"}}).HasScope("anything") {
		t.Error("wildcard scope must match everything")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc", "abc") {
		t.Error("equal strings must match")
	}
	if ConstantTimeEqual("abc", "abd") {
		t.Error("different strings must not match")
	}
}
