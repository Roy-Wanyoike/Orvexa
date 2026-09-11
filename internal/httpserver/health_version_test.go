package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/buildinfo"
)

// TestHealthEndpointsReportBuildStamp pins the [O-37] (issue #46) additive
// contract: both process health surfaces carry the ldflags-stamped build
// identity, and the pre-existing fields (status/checks) are untouched.
// Deps{} is nil-pool safe by construction (route registration never touches
// storage; readiness reports the truth — 503 degraded with no database).
func TestHealthEndpointsReportBuildStamp(t *testing.T) {
	origV, origC, origD := buildinfo.Version, buildinfo.Commit, buildinfo.Date
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = "v0.10.0", "a76b462", "2026-09-10T00:00:00Z"
	defer func() { buildinfo.Version, buildinfo.Commit, buildinfo.Date = origV, origC, origD }()

	h := New(Deps{})

	for _, tc := range []struct {
		path string
		code int
	}{
		{"/healthz", http.StatusOK},                // liveness: always 200
		{"/readyz", http.StatusServiceUnavailable}, // nil pool ⇒ honest "degraded"
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.code {
			t.Fatalf("GET %s = %d, want %d", tc.path, rec.Code, tc.code)
		}
		// httpx.WriteJSON wraps every body in the uniform envelope: {data, meta}.
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s body unmarshal: %v", tc.path, err)
		}
		v, ok := body.Data["version"].(string)
		if !ok {
			t.Fatalf("GET %s: additive version field missing or not a string: %s", tc.path, rec.Body.String())
		}
		for _, want := range []string{"v0.10.0", "a76b462", "2026-09-10T00:00:00Z"} {
			if !strings.Contains(v, want) {
				t.Fatalf("GET %s version = %q, missing %q", tc.path, v, want)
			}
		}
	}
}
