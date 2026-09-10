package identity

import (
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Typed error surface for the identity plane. Codes are stable machine
// contracts (see docs/rbac.md §error taxonomy). No error below ever carries
// token material, signatures or claim payloads — reasons are coarse machine
// words so clients can distinguish failure classes without leaking details
// useful to forgers (an attacker learns why a class failed, never which byte).
func invalidToken(reason string) *apperrors.Error {
	return apperrors.Unauth("identity.invalid_token", "invalid or unacceptable identity token").
		WithDetails(map[string]string{"reason": reason})
}

func errDisabled() *apperrors.Error {
	return apperrors.NotFound("identity.disabled", "OIDC identity is not configured on this deployment")
}

func errDiscovery(cause error) *apperrors.Error {
	return apperrors.Internal("identity.discovery_failed", "identity provider discovery failed").WithCause(cause)
}

func errBindingLookup(cause error) *apperrors.Error {
	return apperrors.Internal("identity.binding_lookup_failed", "authorization lookup failed").WithCause(cause)
}

func capabilityMissing(capability string) *apperrors.Error {
	return apperrors.Forbidden("identity.capability_missing", "missing required capability").
		WithDetails(map[string]string{"required_capability": capability})
}
