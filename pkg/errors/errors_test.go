package errors

import (
	"errors"
	"strings"
	"testing"
)

func TestKindsMapToHTTPStatuses(t *testing.T) {
	cases := map[Kind]int{
		KindInvalid:     422,
		KindNotFound:    404,
		KindConflict:    409,
		KindRateLimited: 429,
		KindUnauth:      401,
		KindForbidden:   403,
		KindInternal:    500,
	}
	for k, want := range cases {
		e := New(k, "test.code", "m")
		if got := e.HTTPStatus(); got != want {
			t.Errorf("kind %s: got %d want %d", k, got, want)
		}
	}
}

func TestFromPassesThroughAppErrors(t *testing.T) {
	orig := Conflict("interaction.invalid_transition", "cannot reopen")
	e := From(orig)
	if e != orig {
		t.Fatal("application errors must pass through unchanged")
	}
}

func TestFromWrapsUnknownAsOpaqueInternal(t *testing.T) {
	secret := errors.New("password=hunter2 host=10.0.0.1")
	e := From(secret)
	if e.Kind != KindInternal {
		t.Fatal("unknown errors must map to internal")
	}
	if strings.Contains(e.Message, "hunter2") {
		t.Fatalf("internal details leaked: %s", e.Message)
	}
	if e.wrapped != secret {
		t.Fatal("cause must be preserved for server-side logging")
	}
}

func TestWrapPreservesChain(t *testing.T) {
	cause := errors.New("pq: deadlock")
	w := Wrap(cause, KindConflict, "db.deadlock", "resource busy")
	if !errors.Is(w, cause) {
		t.Fatal("wrapped cause must remain discoverable via errors.Is")
	}
	if w.HTTPStatus() != 409 {
		t.Fatal("wrap must keep kind mapping")
	}
}

func TestWithDetailsCarriesStructuredPayload(t *testing.T) {
	e := Invalid("interaction.invalid_transition", "bad transition").
		WithDetails(map[string]any{"from": "closed", "requested": "completed"})
	d, ok := e.Details.(map[string]any)
	if !ok || d["from"] != "closed" {
		t.Fatal("details lost")
	}
}
