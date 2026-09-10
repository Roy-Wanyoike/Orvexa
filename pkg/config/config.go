// Package config loads Orvexa process configuration from the environment.
// Every setting has a safe local default so the platform boots with zero
// external dependencies; production overrides come purely from env.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// BusDriver selects the event bus implementation.
type BusDriver string

const (
	BusInproc BusDriver = "inproc"
	BusNATS   BusDriver = "nats"
)

// AIProvider selects the AI gateway provider implementation.
type AIProvider string

const (
	AIRules AIProvider = "rules" // deterministic, no external calls
	AILLM   AIProvider = "llm"   // OpenAI-compatible endpoint via env
)

// Config is the process-wide configuration.
type Config struct {
	HTTPAddr     string
	RealtimeAddr string
	DatabaseURL  string
	DBMaxConns   int32
	BusDriver    BusDriver
	NATSURL      string
	LogLevel     string
	Env          string

	BootstrapAPIKey string

	AIProvider     AIProvider
	AILLMBaseURL   string
	AILLMAPIKey    string
	AIMaxTokens    int
	AITimeout      time.Duration

	CommsProvider     string
	WebhookHMACSecret string

	// Realtime connection caps.
	WSMaxPerPrincipal int
	WSMaxTotal        int
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:     env("ORVEXA_HTTP_ADDR", ":8080"),
		RealtimeAddr: env("ORVEXA_REALTIME_ADDR", ":8081"),
		DatabaseURL:  env("ORVEXA_DATABASE_URL", ""),
		DBMaxConns:   int32(envInt("ORVEXA_DB_MAX_CONNS", 20)),
		BusDriver:    BusDriver(env("ORVEXA_BUS_DRIVER", string(BusInproc))),
		NATSURL:      env("ORVEXA_NATS_URL", "nats://localhost:4222"),
		LogLevel:     env("ORVEXA_LOG_LEVEL", "info"),
		Env:          env("ORVEXA_ENV", "development"),

		BootstrapAPIKey: os.Getenv("ORVEXA_BOOTSTRAP_API_KEY"),

		AIProvider:   AIProvider(env("ORVEXA_AI_PROVIDER", string(AIRules))),
		AILLMBaseURL: os.Getenv("ORVEXA_AI_LLM_BASE_URL"),
		AILLMAPIKey:  os.Getenv("ORVEXA_AI_LLM_API_KEY"),
		AIMaxTokens:  envInt("ORVEXA_AI_MAX_TOKENS", 1024),
		AITimeout:    time.Duration(envInt("ORVEXA_AI_TIMEOUT_SECONDS", 30)) * time.Second,

		CommsProvider:     env("ORVEXA_COMMS_PROVIDER", "simulator"),
		WebhookHMACSecret: os.Getenv("ORVEXA_WEBHOOK_HMAC_SECRET"),

		WSMaxPerPrincipal: envInt("ORVEXA_WS_MAX_PER_PRINCIPAL", 10),
		WSMaxTotal:        envInt("ORVEXA_WS_MAX_TOTAL", 10000),
	}

	switch c.BusDriver {
	case BusInproc, BusNATS:
	default:
		return c, fmt.Errorf("invalid ORVEXA_BUS_DRIVER %q (want inproc|nats)", c.BusDriver)
	}
	if c.AITimeout <= 0 {
		return c, fmt.Errorf("ORVEXA_AI_TIMEOUT_SECONDS must be positive")
	}
	if c.WSMaxPerPrincipal <= 0 || c.WSMaxTotal <= 0 {
		return c, fmt.Errorf("websocket caps must be positive")
	}
	return c, nil
}

// Production reports whether the process runs with production posture.
func (c Config) Production() bool { return c.Env == "production" }
