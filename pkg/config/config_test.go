package config

import (
	"testing"
	"time"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"ORVEXA_HTTP_ADDR", "ORVEXA_BUS_DRIVER", "ORVEXA_AI_PROVIDER",
		"ORVEXA_AI_TIMEOUT_SECONDS", "ORVEXA_WS_MAX_PER_PRINCIPAL", "ORVEXA_WS_MAX_TOTAL",
		"ORVEXA_TELEPHONY_PROVIDER", "ORVEXA_MESSAGING_PROVIDER"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaultsAreRunnable(t *testing.T) {
	withEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("defaults must load: %v", err)
	}
	if c.BusDriver != BusInproc {
		t.Fatalf("default bus must be inproc, got %s", c.BusDriver)
	}
	if c.AIProvider != AIRules {
		t.Fatalf("default ai provider must be rules, got %s", c.AIProvider)
	}
	if c.AITimeout != 30*time.Second {
		t.Fatalf("default ai timeout wrong: %s", c.AITimeout)
	}
	if c.WSMaxPerPrincipal != 10 || c.WSMaxTotal != 10000 {
		t.Fatalf("ws caps defaults wrong: %d/%d", c.WSMaxPerPrincipal, c.WSMaxTotal)
	}
}

func TestLoadRejectsBadBusDriver(t *testing.T) {
	withEnv(t, map[string]string{"ORVEXA_BUS_DRIVER": "kafka"})
	if _, err := Load(); err == nil {
		t.Fatal("invalid bus driver must be rejected")
	}
}

func TestLoadRejectsNonPositiveAITimeout(t *testing.T) {
	withEnv(t, map[string]string{"ORVEXA_AI_TIMEOUT_SECONDS": "0"})
	if _, err := Load(); err == nil {
		t.Fatal("zero ai timeout must be rejected")
	}
}

func TestLoadProviderSelectionKeys(t *testing.T) {
	withEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("defaults must load: %v", err)
	}
	if c.TelephonyProvider != "simulator" || c.MessagingProvider != "simulator" {
		t.Fatalf("provider defaults must be simulator, got telephony=%q messaging=%q",
			c.TelephonyProvider, c.MessagingProvider)
	}

	// Names only: pkg/config stays dependency-free and does not validate
	// provider values; internal/comms/registry is the validation authority.
	withEnv(t, map[string]string{
		"ORVEXA_TELEPHONY_PROVIDER": "twilio",
		"ORVEXA_MESSAGING_PROVIDER": "whatsappcloud",
	})
	c, err = Load()
	if err != nil {
		t.Fatalf("provider selection must load: %v", err)
	}
	if c.TelephonyProvider != "twilio" || c.MessagingProvider != "whatsappcloud" {
		t.Fatalf("provider selection must round-trip, got telephony=%q messaging=%q",
			c.TelephonyProvider, c.MessagingProvider)
	}
}
