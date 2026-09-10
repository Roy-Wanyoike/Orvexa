// Package idempotency provides key derivation and storage-contract helpers so
// externally-triggered operations (provider webhooks, retries, double clicks)
// execute at most once.
//
// The pattern: every externally-triggered operation carries an idempotency key
// — either client-supplied or derived from stable provider identifiers — and
// the persistence layer enforces uniqueness. This package owns derivation and
// validation; SQL migrations own the unique constraints.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var keyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// Validate enforces the client-supplied key format so keys are safe as SQL
// identifiers, log fields and cache keys.
func Validate(key string) error {
	if !keyRe.MatchString(key) {
		return fmt.Errorf("idempotency key must match [A-Za-z0-9._:-]{8,128}")
	}
	return nil
}

// Derive builds a deterministic key from stable parts (e.g. provider + event id).
// Parts are joined with "|" and hashed so derived keys never leak raw inputs
// and always satisfy Validate.
func Derive(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "auto_" + hex.EncodeToString(h[:16])
}
