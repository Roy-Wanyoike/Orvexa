package webhooks

import (
	"net/http"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// atCase mirrors one case of testdata/africastalking_allowlist.json — the
// AT decision matrix (AT documents NO signature/checksum surface, so the
// fixture is the peer-IP allowlist behavior table).
type atCase struct {
	Name         string   `json:"name"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	PeerIPHeader string   `json:"peer_ip_header"`
	HeaderValue  string   `json:"header_value"`
	Expect       string   `json:"expect"`
}

// warnRecorder captures structured warnings emitted by the AT verifier.
type warnRecorder struct {
	msgs []string
	kvs  [][]any
}

func (w *warnRecorder) logger(msg string, kv ...any) {
	w.msgs = append(w.msgs, msg)
	w.kvs = append(w.kvs, kv)
}

func (w *warnRecorder) count() int { return len(w.msgs) }

// TestATAfricaAllowlistDecisionMatrix executes the fixture decision matrix
// from testdata/africastalking_allowlist.json case by case.
func TestATAfricaAllowlistDecisionMatrix(t *testing.T) {
	var fixture struct {
		Cases []atCase `json:"cases"`
	}
	loadJSONFixture(t, "africastalking_allowlist.json", &fixture)
	if len(fixture.Cases) == 0 {
		t.Fatal("no decision-matrix cases")
	}

	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			var warns warnRecorder
			cfg := VerifierConfig{
				ATAllowedCIDRs: tc.AllowedCIDRs,
				ATPeerIPHeader: tc.PeerIPHeader,
				Logger:         warns.logger,
			}
			header := http.Header{}
			if tc.HeaderValue != "" {
				header.Set(tc.PeerIPHeader, tc.HeaderValue)
			}

			err := VerifyAfricasTalking(cfg, []byte(`{"notification":"delivered"}`), header, "")
			switch tc.Expect {
			case "allow":
				if err != nil {
					t.Fatalf("expected allow, got %v", err)
				}
			case "allow-with-structured-warn":
				if err != nil {
					t.Fatalf("expected allow-with-warn, got %v", err)
				}
				if warns.count() != 1 {
					t.Fatalf("expected exactly one structured residual-risk warning, got %d", warns.count())
				}
			case "reject":
				if err == nil {
					t.Fatal("expected reject (fail-closed), got nil error (fail-open!)")
				}
				appErr, ok := err.(*apperrors.Error)
				if !ok || appErr.Kind != apperrors.KindUnauth {
					t.Fatalf("expected 401-class rejection, got %v", err)
				}
			default:
				t.Fatalf("unknown expectation %q", tc.Expect)
			}
		})
	}
}

// TestATRightmostEntryTrustModel pins the peer-identity rule: the RIGHTMOST
// X-Forwarded-For entry decides. Client-asserted leftmost entries are
// discarded — a forged claim cannot fake allowlist membership behind an
// appending edge. The only unrescuable topology is direct exposure (no
// edge), where the whole header is attacker-controlled: that residual risk
// is documented, not silently handled.
func TestATRightmostEntryTrustModel(t *testing.T) {
	base := VerifierConfig{ATAllowedCIDRs: []string{"196.250.209.0/24"}}

	cases := []struct {
		name   string
		xff    string
		accept bool
	}{
		// Overwriting edge: single entry = true peer.
		{"overwrite-edge-true-peer", "196.250.209.77", true},
		// Appending edge: "forged-claim, true-peer" — forged leftmost claim
		// is DISCARDED, rightmost (edge-observed) decides.
		{"appending-edge-forged-leftmost-discarded", "196.250.209.77, 203.0.113.9", false},
		{"appending-edge-true-peer-rightmost", "203.0.113.9, 196.250.209.77", true},
		// Multi-proxy chain: rightmost is the last hop's observation.
		{"proxy-chain-rightmost-decides", "10.0.0.1, 10.0.0.2, 196.250.209.77", true},
		{"proxy-chain-rightmost-outside", "196.250.209.77, 10.0.0.2, 203.0.113.9", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{"X-Forwarded-For": []string{tc.xff}}
			err := VerifyAfricasTalking(base, []byte(`{}`), header, "")
			if tc.accept && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
			if !tc.accept {
				requireUnauthReject(t, err, atCodePeerNotAllowed)
			}
		})
	}
}

// TestATFailClosed: with an allowlist configured, missing/garbage/zoned peer
// identity must reject — authenticity cannot be established, so the delivery
// is rejected (never degraded to accept).
func TestATFailClosed(t *testing.T) {
	cfg := VerifierConfig{ATAllowedCIDRs: []string{"10.0.0.0/8"}}

	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"missing-peer-header", "X-Forwarded-For", ""},
		{"garbage-peer", "X-Forwarded-For", "not-an-ip"},
		{"zoned-address", "X-Forwarded-For", "fe80::1%eth0"},
		{"empty-rightmost-entry", "X-Forwarded-For", "10.0.0.5,"},
		{"only-commas", "X-Forwarded-For", ",,,"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.value != "" {
				header.Set(tc.header, tc.value)
			}
			err := VerifyAfricasTalking(cfg, []byte(`{}`), header, "")
			if err == nil {
				t.Fatal("expected fail-closed rejection")
			}
			appErr := err.(*apperrors.Error)
			if appErr.Code != atCodePeerUnverifiable && appErr.Code != atCodePeerNotAllowed {
				t.Fatalf("unexpected code %q", appErr.Code)
			}
		})
	}
}

// TestATMalformedCIDRsFailClosed: netip.ParsePrefix ACCEPTS host bits set
// ("10.0.0.1/8") — the verifier must disable such an entry (fail-closed)
// instead of silently widening it to its masked network. An allowlist made
// entirely of garbage rejects every delivery.
func TestATMalformedCIDRsFailClosed(t *testing.T) {
	var warns warnRecorder

	t.Run("host-bits-set-entry-disabled", func(t *testing.T) {
		// Without the strict check, 10.0.0.1/8 would effectively match the
		// whole 10.0.0.0/8 — a config typo widening the allowlist.
		cfg := VerifierConfig{
			ATAllowedCIDRs: []string{"10.0.0.1/8"},
			Logger:         warns.logger,
		}
		header := http.Header{"X-Forwarded-For": []string{"10.0.0.1"}}
		requireUnauthReject(t, VerifyAfricasTalking(cfg, []byte(`{}`), header, ""), atCodePeerNotAllowed)
		if warns.count() == 0 {
			t.Fatal("disabled entry must be logged")
		}
	})

	t.Run("all-entries-garbage-rejects", func(t *testing.T) {
		cfg := VerifierConfig{ATAllowedCIDRs: []string{"garbage", "", "300.1.2.3/24"}}
		header := http.Header{"X-Forwarded-For": []string{"10.0.0.5"}}
		requireUnauthReject(t, VerifyAfricasTalking(cfg, []byte(`{}`), header, ""), atCodePeerNotAllowed)
	})

	t.Run("allowlist-matching-nothing-rejects", func(t *testing.T) {
		cfg := VerifierConfig{ATAllowedCIDRs: []string{"192.0.2.0/24"}}
		header := http.Header{"X-Forwarded-For": []string{"10.0.0.5"}}
		requireUnauthReject(t, VerifyAfricasTalking(cfg, []byte(`{}`), header, ""), atCodePeerNotAllowed)
	})

	t.Run("valid-cidr-without-host-bits-still-matches", func(t *testing.T) {
		cfg := VerifierConfig{ATAllowedCIDRs: []string{"10.0.0.0/8"}}
		header := http.Header{"X-Forwarded-For": []string{"10.1.2.3"}}
		if err := VerifyAfricasTalking(cfg, []byte(`{}`), header, ""); err != nil {
			t.Fatalf("valid masked CIDR must keep matching: %v", err)
		}
	})
}

// TestATWarnOnUnconfiguredAllowlist: without an allowlist the delivery is
// accepted WITHOUT cryptographic verification — deliberately, with a
// structured residual-risk warning on EVERY acceptance (honest, not a
// silent bypass).
func TestATWarnOnUnconfiguredAllowlist(t *testing.T) {
	var warns warnRecorder
	cfg := VerifierConfig{Logger: warns.logger}
	header := http.Header{"X-Forwarded-For": []string{"196.250.209.77"}}

	for i := 0; i < 3; i++ {
		if err := VerifyAfricasTalking(cfg, []byte(`{"event":"sms.delivered"}`), header, ""); err != nil {
			t.Fatalf("unconfigured allowlist must accept (documented residual risk): %v", err)
		}
	}
	if warns.count() != 3 {
		t.Fatalf("expected one warning per acceptance (3), got %d", warns.count())
	}

	// The warning is structured, fixed-message, and carries no payload or
	// header contents.
	msg := warns.msgs[0]
	if msg != "africastalking webhook accepted without cryptographic verification" {
		t.Fatalf("unexpected warning message: %q", msg)
	}
	for _, kv := range warns.kvs[0] {
		if s, ok := kv.(string); ok {
			for _, leak := range []string{"196.250.209.77", "sms.delivered", "X-Forwarded-For"} {
				if s == leak {
					t.Fatalf("warning must not carry request data: %q", s)
				}
			}
		}
	}
}

// TestATCustomPeerHeader: the peer-identity header is configurable, for
// deployments whose edge writes a dedicated header (e.g. a custom header
// only the edge sets — the documented mitigation for non-overwriting
// edges).
func TestATCustomPeerHeader(t *testing.T) {
	cfg := VerifierConfig{
		ATAllowedCIDRs: []string{"203.0.113.0/24"},
		ATPeerIPHeader: "Cf-Connecting-Ip",
	}

	// The custom header decides — XFF is ignored entirely.
	header := http.Header{
		"X-Forwarded-For":  []string{"10.0.0.1"},
		"Cf-Connecting-Ip": []string{"203.0.113.7"},
	}
	if err := VerifyAfricasTalking(cfg, []byte(`{}`), header, ""); err != nil {
		t.Fatalf("custom peer header must decide: %v", err)
	}

	header.Set("Cf-Connecting-Ip", "198.51.100.9")
	requireUnauthReject(t, VerifyAfricasTalking(cfg, []byte(`{}`), header, ""), atCodePeerNotAllowed)
}

// TestATIPv4MappedIPv6Peer: IPv4-mapped IPv6 peers are unmapped so they
// compare deterministically against IPv4 CIDRs.
func TestATIPv4MappedIPv6Peer(t *testing.T) {
	cfg := VerifierConfig{ATAllowedCIDRs: []string{"10.0.0.0/8"}}
	header := http.Header{"X-Forwarded-For": []string{"::ffff:10.20.30.40"}}
	if err := VerifyAfricasTalking(cfg, []byte(`{}`), header, ""); err != nil {
		t.Fatalf("IPv4-mapped IPv6 peer must match IPv4 CIDR: %v", err)
	}
}

// TestATReplayIsTransportOriginOnly: AT has no signature surface, so a
// replay of the same payload from an ALLOWED peer is transport-authentic by
// definition — the residual risk is documented; payload-level identity is
// the adapter's cross-check responsibility (ADR-0005). This test pins the
// documented behavior so it can never silently tighten or loosen.
func TestATReplayIsTransportOriginOnly(t *testing.T) {
	cfg := VerifierConfig{ATAllowedCIDRs: []string{"10.0.0.0/8"}}
	body := []byte(`{"eventType":"Sent","request":{"x":1}}`)
	header := http.Header{"X-Forwarded-For": []string{"10.0.0.5"}}
	for i := 0; i < 2; i++ {
		if err := VerifyAfricasTalking(cfg, body, header, ""); err != nil {
			t.Fatalf("allowed peer replays are transport-authentic (payload identity is downstream): %v", err)
		}
	}
}
