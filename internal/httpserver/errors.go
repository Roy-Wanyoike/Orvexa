package httpserver

import (
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Pre-constructed static errors for transport-layer rejections. These never
// vary per-request so no allocation or context leakage occurs.
var (
	errWebhookUnavailable   = apperrors.Unauth("webhook.not_configured", "webhook ingestion is not configured on this deployment")
	errWebhookRateLimited   = apperrors.RateLimited("webhook.rate_limited", "too many webhook submissions; slow down")
	errWebhookBodyUnreadable = apperrors.Invalid("webhook.body_unreadable", "could not read request body")
)
