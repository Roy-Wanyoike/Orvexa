//go:build temporal

package temporaldriver

import "testing"

func TestFromEnvDefaults(t *testing.T) {
	for _, k := range []string{EnvURL, EnvNamespace, EnvTaskQueue, EnvSource} {
		t.Setenv(k, "")
	}
	got := FromEnv()
	if got.HostPort != "" {
		t.Fatalf("HostPort: expected empty (Dial rejects loudly), got %q", got.HostPort)
	}
	if got.Namespace != DefaultNamespace {
		t.Fatalf("Namespace: expected %q, got %q", DefaultNamespace, got.Namespace)
	}
	if got.TaskQueue != DefaultTaskQueue {
		t.Fatalf("TaskQueue: expected %q, got %q", DefaultTaskQueue, got.TaskQueue)
	}
	if got.Source != DefaultSource {
		t.Fatalf("Source: expected %q, got %q", DefaultSource, got.Source)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv(EnvURL, "temporal.internal:7233")
	t.Setenv(EnvNamespace, "orvexa-prod")
	t.Setenv(EnvTaskQueue, "wf-prod")
	t.Setenv(EnvSource, "orvexa-worker")
	got := FromEnv()
	if got.HostPort != "temporal.internal:7233" || got.Namespace != "orvexa-prod" ||
		got.TaskQueue != "wf-prod" || got.Source != "orvexa-worker" {
		t.Fatalf("overrides not honored: %+v", got)
	}
}

func TestFromEnvStripsScheme(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:7233/":  "127.0.0.1:7233",
		"https://127.0.0.1:7233":  "127.0.0.1:7233",
		"grpc://127.0.0.1:7233/":  "127.0.0.1:7233",
		"  127.0.0.1:7233  ":      "127.0.0.1:7233",
		"127.0.0.1:7233":          "127.0.0.1:7233",
		"postgresql://x:y@h:1/db": "postgresql://x:y@h:1/db", // unknown schemes untouched
	}
	for in, want := range cases {
		t.Setenv(EnvURL, in)
		if got := FromEnv().HostPort; got != want {
			t.Fatalf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigWithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Namespace != DefaultNamespace || got.TaskQueue != DefaultTaskQueue || got.Source != DefaultSource {
		t.Fatalf("zero Config should inherit defaults, got %+v", got)
	}
	if got.HostPort != "" {
		t.Fatalf("HostPort must stay empty (caller decides loud failure), got %q", got.HostPort)
	}
}
