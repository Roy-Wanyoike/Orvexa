package registry

import (
	"errors"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func TestNewGlobalResolver(t *testing.T) {
	tel := &TelephonyConfig{Provider: ProviderSimulator}
	msg := &MessagingConfig{Provider: ProviderSimulator}
	r := NewGlobalResolver(tel, msg)

	gotTel, gotMsg, err := r("tenant-42")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotTel == tel || gotMsg == msg {
		t.Fatal("resolver must return defensive clones, not shared pointers")
	}
	if gotTel.Provider != ProviderSimulator || gotMsg.Provider != ProviderSimulator {
		t.Fatalf("global configs must resolve for tenants, got %q/%q", gotTel.Provider, gotMsg.Provider)
	}

	// Mutating a resolved config must never corrupt global state.
	gotTel.Provider = ProviderTwilio
	if tel.Provider != ProviderSimulator {
		t.Fatal("clone mutation leaked into the global configuration")
	}

	for _, id := range []string{"", "   "} {
		tel2, msg2, err := r(id)
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.tenant_required" {
			t.Fatalf("empty tenant id must yield comms.tenant_required, got %v", err)
		}
		if tel2 != nil || msg2 != nil {
			t.Fatal("rejected resolutions must return nil configs")
		}
	}
}

func TestNewGlobalResolverNilPlanes(t *testing.T) {
	r := NewGlobalResolver(nil, &MessagingConfig{Provider: ProviderSimulator})
	tel, msg, err := r("tenant-7")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tel != nil {
		t.Fatal("nil telephony plane must resolve to nil")
	}
	if msg == nil || msg.Provider != ProviderSimulator {
		t.Fatal("configured plane must resolve")
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv(EnvTelephonyProvider, "twilio")
	for _, k := range []string{"ORVEXA_TWILIO_ACCOUNT_SID", "ORVEXA_TWILIO_AUTH_TOKEN", "ORVEXA_TWILIO_FROM_NUMBER"} {
		t.Setenv(k, fakeProviderEnv[k])
	}
	t.Setenv(EnvMessagingProvider, "whatsappcloud")
	for _, k := range []string{"ORVEXA_WHATSAPP_PHONE_NUMBER_ID", "ORVEXA_WHATSAPP_ACCESS_TOKEN", "ORVEXA_WHATSAPP_APP_SECRET", "ORVEXA_WHATSAPP_VERIFY_TOKEN"} {
		t.Setenv(k, fakeProviderEnv[k])
	}

	tel, msg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if tel.Provider != ProviderTwilio || msg.Provider != ProviderWhatsAppCloud {
		t.Fatalf("providers = %q/%q, want twilio/whatsappcloud", tel.Provider, msg.Provider)
	}
	if tel.TwilioAuthToken != Secret(fakeProviderEnv["ORVEXA_TWILIO_AUTH_TOKEN"]) {
		t.Fatal("telephony credentials must round-trip through os env")
	}
	if msg.WhatsAppAppSecret != Secret(fakeProviderEnv["ORVEXA_WHATSAPP_APP_SECRET"]) {
		t.Fatal("messaging credentials must round-trip through os env")
	}
}

func TestLoadFromEnvFailsFastOnFirstError(t *testing.T) {
	t.Setenv(EnvTelephonyProvider, "plivo")
	tel, msg, err := LoadFromEnv()
	if err == nil {
		t.Fatal("unknown provider must fail")
	}
	if tel != nil || msg != nil {
		t.Fatal("failed loads must return nil configs")
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Code != "comms.unknown_provider" {
		t.Fatalf("want comms.unknown_provider, got %v", err)
	}
}
