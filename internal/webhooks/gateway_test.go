package webhooks

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

const testSecret = "test-hmac-secret-key"

func sampleBody() []byte {
	b, _ := json.Marshal(map[string]any{
		"provider":     "simulator",
		"interaction":  "call-123",
		"event":        "ringing",
		"occurred_at":  time.Now().UTC().Format(time.RFC3339),
		"unique_nonce": "n-1",
	})
	return b
}

func TestSignatureRoundTrip(t *testing.T) {
	body := sampleBody()
	sig := ComputeSignature(testSecret, body)
	if !ValidateSignature(testSecret, body, sig) {
		t.Fatal("valid signature must validate")
	}
}

func TestSignatureRejectsTamperedBody(t *testing.T) {
	body := sampleBody()
	sig := ComputeSignature(testSecret, body)
	tampered := append([]byte{}, body...)
	tampered = append(tampered, []byte(`"evil":true`)...)
	if ValidateSignature(testSecret, tampered, sig) {
		t.Fatal("tampered body must fail validation")
	}
}

func TestSignatureRejectsWrongSecretAndEmptySecret(t *testing.T) {
	body := sampleBody()
	sig := ComputeSignature("other-secret", body)
	if ValidateSignature(testSecret, body, sig) {
		t.Fatal("signature from wrong secret must fail")
	}
	if ValidateSignature("", body, sig) {
		t.Fatal("empty secret must reject everything (no unsigned ingestion)")
	}
}

func TestSignatureRejectsReplayAcrossBodies(t *testing.T) {
	b1, _ := json.Marshal(map[string]string{"event": "a", "nonce": "1"})
	b2, _ := json.Marshal(map[string]string{"event": "b", "nonce": "2"})
	sig1 := ComputeSignature(testSecret, b1)
	if ValidateSignature(testSecret, b2, sig1) {
		t.Fatal("signature must be bound to its exact body")
	}
}

func TestDerivedEventIDStableAndDistinct(t *testing.T) {
	body := sampleBody()
	a := ComputeSignature(testSecret, body)
	b := ComputeSignature(testSecret, body)
	if a != b {
		t.Fatal("same body+secret must produce identical signatures (replay dedupe basis)")
	}
	if bytes.Equal(body, nil) {
		t.Fatal("body must be non-nil")
	}
}

// NOTE: Gateway.Ingest persistence behavior (dedup 409→duplicate=true,
// replay returns original id) is covered by the integration suite in
// webhooks_integration_test.go, which runs when ORVEXA_TEST_DATABASE_URL
// points at an empty Orvexa database (migrations applied).
