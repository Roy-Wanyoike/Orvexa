package idempotency

import (
	"strings"
	"testing"
)

func TestValidateAcceptsWellFormedKeys(t *testing.T) {
	for _, k := range []string{"twilio:UOabc123:def456", "client-key-1", "2026.09.10.run", "aVery_Long:key-42"} {
		if err := Validate(k); err != nil {
			t.Errorf("key %q should pass: %v", k, err)
		}
	}
}

func TestValidateRejectsBadKeys(t *testing.T) {
	for _, k := range []string{"", "short", "has spaces", "semicolon;", "quote\"", strings.Repeat("x", 129)} {
		if err := Validate(k); err == nil {
			t.Errorf("key %q should be rejected", k)
		}
	}
}

func TestDeriveIsDeterministicAndDistinct(t *testing.T) {
	a1 := Derive("twilio", "evt_100")
	a2 := Derive("twilio", "evt_100")
	b := Derive("twilio", "evt_101")
	c := Derive("twilio", "evt_10", "extra")
	if a1 != a2 {
		t.Fatal("derive must be deterministic")
	}
	if a1 == b || a1 == c {
		t.Fatal("different inputs must derive different keys")
	}
	if err := Validate(a1); err != nil {
		t.Fatalf("derived key must satisfy Validate: %v", err)
	}
	if !strings.HasPrefix(a1, "auto_") {
		t.Fatal("derived keys must be marked auto_")
	}
}
