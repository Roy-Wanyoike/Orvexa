//go:build temporal

package temporaldriver

import (
	"os"
	"strings"
)

// Environment variables understood by FromEnv. The URL is the Temporal
// frontend gRPC endpoint as host:port (e.g. 127.0.0.1:7233); a URL-style
// scheme prefix is tolerated and stripped. The same variable gates the
// integration test (tests skip cleanly when it is unset).
const (
	EnvURL       = "ORVEXA_TEMPORAL_URL"          // host:port of the Temporal frontend (gRPC)
	EnvNamespace = "ORVEXA_TEMPORAL_NAMESPACE"    // Temporal namespace (default: "default")
	EnvTaskQueue = "ORVEXA_TEMPORAL_TASK_QUEUE"   // workflow+activity task queue
	EnvSource    = "ORVEXA_WORKFLOWS_SOURCE"      // outbox event source for audit parity
)

// Defaults keep zero-config dev stacks predictable; every field is overridable
// by environment. No credentials are read here — mTLS/API-key material is a
// deployment concern (see ADR-0009 Security).
const (
	DefaultNamespace = "default"
	DefaultTaskQueue = "orvexa-workflows"
	DefaultSource    = "orvexa-temporal"
)

// Config is the driver's connection and naming configuration.
type Config struct {
	HostPort  string // Temporal frontend host:port (EnvURL)
	Namespace string // Temporal namespace
	TaskQueue string // task queue hosting the driver's workflows and activities
	Source    string // source stamped on outbox envelopes (audit parity with the engine)
}

// FromEnv builds a Config from the environment, applying defaults for unset
// optional fields. HostPort may still be empty (Dial rejects it loudly).
func FromEnv() Config {
	return Config{
		HostPort:  normalizeURL(os.Getenv(EnvURL)),
		Namespace: firstNonEmpty(os.Getenv(EnvNamespace), DefaultNamespace),
		TaskQueue: firstNonEmpty(os.Getenv(EnvTaskQueue), DefaultTaskQueue),
		Source:    firstNonEmpty(os.Getenv(EnvSource), DefaultSource),
	}
}

// withDefaults fills empty fields so callers constructing Config literally
// get the same contract as FromEnv.
func (c Config) withDefaults() Config {
	return Config{
		HostPort:  normalizeURL(c.HostPort),
		Namespace: firstNonEmpty(c.Namespace, DefaultNamespace),
		TaskQueue: firstNonEmpty(c.TaskQueue, DefaultTaskQueue),
		Source:    firstNonEmpty(c.Source, DefaultSource),
	}
}

// normalizeURL strips a tolerated scheme prefix. Temporal's gRPC endpoint has
// no scheme; accepting UI-style pastes (http://127.0.0.1:7233) prevents a
// whole class of config typos from surfacing as dial timeouts.
func normalizeURL(v string) string {
	v = strings.TrimSpace(v)
	for _, p := range []string{"https://", "http://", "grpc://"} {
		if strings.HasPrefix(v, p) {
			return strings.TrimSuffix(strings.TrimPrefix(v, p), "/")
		}
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
