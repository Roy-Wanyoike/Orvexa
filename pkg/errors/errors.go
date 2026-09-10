// Package errors defines Orvexa's application error model.
//
// Every error surfaced to a client is an *Error carrying a stable machine code,
// an HTTP status, and an optional human message plus structured details.
// Domain code wraps sentinel kinds; transport layers map kind → HTTP status so
// business logic never imports net/http.
package errors

import (
	"errors"
	"fmt"
	"net/http"
)

// Kind classifies an application error.
type Kind string

const (
	KindInvalid     Kind = "invalid"      // 422 — input failed validation
	KindNotFound    Kind = "not_found"    // 404 — resource does not exist (or is foreign-tenant)
	KindConflict    Kind = "conflict"     // 409 — illegal state transition or duplicate
	KindRateLimited Kind = "rate_limited" // 429 — quota exceeded
	KindUnauth      Kind = "unauthorized" // 401 — missing/invalid credentials
	KindForbidden   Kind = "forbidden"    // 403 — authenticated, not allowed
	KindInternal    Kind = "internal"     // 500 — unexpected; never leaks internals
)

// Error is the canonical application error.
type Error struct {
	Kind    Kind
	Code    string // stable machine code, e.g. "interaction.invalid_transition"
	Message string // safe-for-client human message
	Details any    // structured, safe-for-client metadata
	wrapped error
}

func (e *Error) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.wrapped)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.wrapped }

// HTTPStatus maps the kind to its wire status.
func (e *Error) HTTPStatus() int {
	switch e.Kind {
	case KindInvalid:
		return http.StatusUnprocessableEntity
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindRateLimited:
		return http.StatusTooManyRequests
	case KindUnauth:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// New constructs an application error.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Wrap attaches a cause while keeping the client-safe surface.
func Wrap(err error, kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message, wrapped: err}
}

// WithDetails returns a shallow copy carrying structured details.
func (e *Error) WithDetails(details any) *Error {
	c := *e
	c.Details = details
	return &c
}

// Convenience constructors keep call sites terse and consistent.

func Invalid(code, msg string) *Error     { return New(KindInvalid, code, msg) }
func NotFound(code, msg string) *Error    { return New(KindNotFound, code, msg) }
func Conflict(code, msg string) *Error    { return New(KindConflict, code, msg) }
func RateLimited(code, msg string) *Error { return New(KindRateLimited, code, msg) }
func Unauth(code, msg string) *Error      { return New(KindUnauth, code, msg) }
func Forbidden(code, msg string) *Error   { return New(KindForbidden, code, msg) }
func Internal(code, msg string) *Error    { return New(KindInternal, code, msg) }

// From maps an unknown error to the canonical model. Unknown errors become
// KindInternal with a deliberately opaque message so internals never leak.
func From(err error) *Error {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Internal("internal.error", "unexpected internal error").WithCause(err)
}

// WithCause attaches a private cause (logged server-side, never returned).
func (e *Error) WithCause(cause error) *Error {
	e.wrapped = cause
	return e
}
