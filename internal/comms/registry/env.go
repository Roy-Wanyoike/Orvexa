package registry

import (
	"os"
	"strings"
)

// Selection env vars. The provider NAMES are additionally mirrored
// (additively) in pkg/config for code paths that only need the selection;
// credential parsing and validation happen only here.
const (
	// EnvTelephonyProvider selects the voice-plane provider adapter.
	EnvTelephonyProvider = "ORVEXA_TELEPHONY_PROVIDER"
	// EnvMessagingProvider selects the messaging-plane provider adapter.
	EnvMessagingProvider = "ORVEXA_MESSAGING_PROVIDER"
)

// defaultProvider keeps the platform's zero-dependency boot posture: the
// built-in simulator needs no credentials.
const defaultProvider = ProviderSimulator

// envGetter abstracts the environment for parsing: os in production, plain
// tables in tests.
type envGetter func(key string) string

// TelephonyFromEnv builds and validates the voice-plane configuration from
// the process environment.
func TelephonyFromEnv() (*TelephonyConfig, error) {
	return telephonyFromLookup(func(k string) string { return os.Getenv(k) })
}

// MessagingFromEnv builds and validates the messaging-plane configuration
// from the process environment.
func MessagingFromEnv() (*MessagingConfig, error) {
	return messagingFromLookup(func(k string) string { return os.Getenv(k) })
}

// LoadFromEnv builds and validates both planes; the first failure aborts.
// This is the startup entry point for process wiring.
func LoadFromEnv() (*TelephonyConfig, *MessagingConfig, error) {
	tel, err := TelephonyFromEnv()
	if err != nil {
		return nil, nil, err
	}
	msg, err := MessagingFromEnv()
	if err != nil {
		return nil, nil, err
	}
	return tel, msg, nil
}

// selectedProvider resolves and validates the provider name for one plane.
// Unset or empty values fall back to the simulator; a known name that does
// not serve the plane, or an unknown name, is a startup error.
func selectedProvider(get envGetter, envKey, planeLabel string, p Plane) (ProviderName, error) {
	raw := strings.TrimSpace(get(envKey))
	if raw == "" {
		return defaultProvider, nil
	}
	spec, ok := Lookup(ProviderName(raw))
	if !ok {
		return "", unknownProviderError(planeLabel+" provider", raw)
	}
	if spec.Planes&p == 0 {
		return "", unsupportedPlaneError(spec.Name, p)
	}
	return spec.Name, nil
}
