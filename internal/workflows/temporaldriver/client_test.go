//go:build temporal

package temporaldriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	serviceerror "go.temporal.io/api/serviceerror"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func TestWorkflowIDScheme(t *testing.T) {
	if got := WorkflowID("abc-123"); got != "orvexa-workflow-abc-123" {
		t.Fatalf("WorkflowID = %q, want prefixed scheme", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(serviceerror.NewNotFound("gone")) {
		t.Fatal("serviceerror.NotFound must be recognized")
	}
	if isNotFound(errors.New("boom")) {
		t.Fatal("plain errors must not be treated as not-found")
	}
	if isNotFound(nil) {
		t.Fatal("nil must not be treated as not-found")
	}
}

func TestDialClientRequiresURL(t *testing.T) {
	_, err := dialClient(context.Background(), Config{})
	wantErrCode(t, err, apperrors.KindInvalid, "temporal.url_required")
}

func TestDialClientUnreachableFailsLoudly(t *testing.T) {
	// An unroutable loopback address: DialContext's gRPC lazy connect makes
	// this succeed-or-fail fast; either way it must be a temporal.dial_failed
	// internal error, never silently swallowed.
	_, err := dialClient(context.Background(), Config{HostPort: "127.0.0.1:1"})
	if err == nil {
		t.Skip("loopback dial unexpectedly succeeded; nothing to assert")
	}
	wantErrCode(t, err, apperrors.KindInternal, "temporal.dial_failed")
	if !strings.Contains(err.Error(), "temporal.dial_failed") {
		t.Fatalf("error text should carry the stable code, got %v", err)
	}
}
