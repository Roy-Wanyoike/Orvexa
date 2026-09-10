package registry

import (
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Resolver resolves the provider configuration for one tenant interaction.
//
// Today the default is the global (env-derived) configuration for every
// tenant; the intended evolution is a CRM/tenant-store backed implementation
// returning per-tenant credentials. That future implementation is the
// CRM-override point: it must construct TelephonyConfig / MessagingConfig
// itself and pass them through ValidateTelephony / ValidateMessaging before
// any adapter consumes them.
//
// Contract: implementations return defensive copies (callers may mutate the
// result without corrupting shared state) and never log the raw values —
// renderings are redacted, but the underlying fields are live credentials.
type Resolver func(tenantID string) (*TelephonyConfig, *MessagingConfig, error)

// NewGlobalResolver returns the default Resolver: every valid tenant gets a
// clone of the global configuration. A nil plane argument resolves to nil
// for that plane (adapters treat that as "plane not configured").
func NewGlobalResolver(globalTelephony *TelephonyConfig, globalMessaging *MessagingConfig) Resolver {
	return func(tenantID string) (*TelephonyConfig, *MessagingConfig, error) {
		if strings.TrimSpace(tenantID) == "" {
			return nil, nil, apperrors.Invalid("comms.tenant_required",
				"tenant id is required to resolve provider configuration")
		}
		return globalTelephony.Clone(), globalMessaging.Clone(), nil
	}
}
