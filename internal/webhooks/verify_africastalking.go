package webhooks

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// at rejection codes (stable machine codes, 401-class, fixed messages).
const (
	atCodePeerNotAllowed   = "webhook.at_peer_not_allowed"  // peer outside configured allowlist
	atCodePeerUnverifiable = "webhook.at_peer_unverifiable" // peer identity missing/unparseable
)

// errPrefixHostBitsSet marks an allowlist entry with host bits set
// ("10.0.0.1/8"). Such an entry is DISABLED (fail-closed) rather than
// silently treated as its masked network — a typo must never widen the
// allowlist.
var errPrefixHostBitsSet = errors.New("prefix has host bits set")

// VerifyAfricasTalking enforces the only server-side origin control Africa's
// Talking supports for callbacks: a peer-IP allowlist.
//
// ── RESEARCH FINDING (captured 2026-09, issue #24) ─────────────────────────
// Africa's Talking does NOT sign its standard callback surfaces. The voice
// callback, SMS delivery-report and USSD notification payloads carry no HMAC
// and no checksum header; no signature scheme is documented for them, and no
// checksummed AT callback surface was found to implement. AT's own guidance
// is to restrict callbacks by their published egress IP ranges. This
// verifier therefore implements exactly that control, honestly:
//
//   - Allowlist CONFIGURED (cfg.ATAllowedCIDRs non-empty): the delivery's
//     peer address must parse and match a configured CIDR or the delivery is
//     REJECTED (401-class). Missing/garbage peer identity → REJECTED
//     (fail-closed — identity cannot be established, authenticity cannot be
//     established). Unparseable CIDR entries are skipped but logged, and an
//     allowlist that matches nothing still rejects.
//
//   - Allowlist UNCONFIGURED: the delivery is ACCEPTED WITHOUT any
//     cryptographic verification and a STRUCTURED WARN is logged on every
//     such acceptance. This is a deliberate, honest residual risk — NOT a
//     silent bypass. Operators who cannot allowlist must enforce AT egress
//     ranges at the network layer; if they do neither, callback forgery
//     (spoofed delivery receipts, injected call status) is possible and the
//     logs say so on every accepted delivery.
//
// ── PEER IDENTITY TRUST MODEL (read before wiring) ─────────────────────────
// VerifyFunc sees transport headers, not the socket, so the peer address is
// read from the header named by cfg.ATPeerIPHeader (default
// "X-Forwarded-For") using the RIGHTMOST-ENTRY rule: the peer is the last
// comma-separated entry — the value written by the LAST hop in the chain.
// This is the only header position a single trusted edge controls in every
// topology, adversarially tested:
//
//   - Edge OVERWRITES XFF (nginx `X-Forwarded-For $remote_addr`): exactly one
//     entry, the true peer. SAFE.
//   - Edge APPENDS ($proxy_add_x_forwarded_for, ALB): the header is
//     "<client-asserted junk…>, <true peer>". Forged leftmost entries are
//     DISCARDED — a client-spoofed "10.0.0.1" claim cannot fake allowlist
//     membership (the forged-xff fixture proves the rejection). The
//     rightmost entry is the address the trusted edge itself observed.
//     SAFE (single trusted edge).
//   - No proxy (Orvexa exposed directly): the entire header is
//     attacker-controlled and a lone forged value CAN claim allowlist
//     membership. RESIDUAL RISK — the verifier cannot distinguish a direct
//     connection from a proxied one; deploy behind an overwriting edge,
//     set ATPeerIPHeader to a custom header only the edge sets, or enforce
//     AT egress IPs at the network layer.
//
// Leftmost-entry parsing was deliberately REJECTED: the leftmost value is
// the one the CLIENT sends first, so it is spoofable under every appending
// edge. The rightmost rule is strictly safer; the only topology it cannot
// rescue is direct exposure, which is called out above and warned on.
//
// Scope note: payload identity for AT callbacks is established downstream —
// adapters must cross-check payload tenant/interaction references against
// platform state exactly as the provider-event contract requires (ADR-0005);
// this verifier contributes transport-origin authenticity only.
func VerifyAfricasTalking(cfg VerifierConfig, rawBody []byte, header http.Header, _ string) error {
	if len(cfg.ATAllowedCIDRs) == 0 {
		// Honest residual-risk warning on EVERY unsigned acceptance. Fixed
		// strings + provider metadata only — never payload/header contents.
		cfg.log("africastalking webhook accepted without cryptographic verification",
			"provider", ProviderAfricasTalking,
			"control", "none",
			"residual_risk", "callback forgery possible unless network-layer origin controls exist",
			"remediation", "configure the AT peer-IP allowlist (ATAllowedCIDRs)")
		return nil
	}

	peer, ok := peerAddr(cfg, header)
	if !ok {
		return apperrors.Unauth(atCodePeerUnverifiable, "callback rejected")
	}

	allowed := false
	for i, entry := range cfg.ATAllowedCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
		if err == nil && prefix != prefix.Masked() {
			// netip.ParsePrefix ACCEPTS host bits set ("10.0.0.1/8") and
			// Contains() then matches as if the host bits were zero — a
			// typo would silently widen the entry to its full network.
			// Enforce the strict reading instead: disable the entry and
			// log (fail-closed).
			err = errPrefixHostBitsSet
		}
		if err != nil {
			// Garbage, empty, or host-bits-set entries disable that entry
			// (fail-closed) and are logged — sloppy configuration can
			// never widen the allowlist. An allowlist that matches
			// nothing still rejects every delivery.
			cfg.log("africastalking allowlist entry unparseable; entry disabled",
				"provider", ProviderAfricasTalking, "entry_index", i)
			continue
		}
		if prefix.Contains(peer) {
			allowed = true
			break
		}
	}
	if !allowed {
		cfg.log("africastalking peer rejected by allowlist",
			"provider", ProviderAfricasTalking, "peer", peer.String())
		return apperrors.Unauth(atCodePeerNotAllowed, "callback rejected")
	}
	return nil
}

// peerAddr extracts and parses the peer IP from the configured header using
// the RIGHTMOST comma-separated entry — the value written by the last hop,
// which a single trusted edge controls in both overwrite and append modes;
// client-asserted leftmost entries are ignored (spoof-proof under appending
// edges). IPv4-mapped IPv6 peers are unmapped so they compare against IPv4
// CIDRs deterministically. Zoned or unparseable addresses fail closed.
func peerAddr(cfg VerifierConfig, header http.Header) (netip.Addr, bool) {
	v := header.Get(cfg.peerIPHeaderName())
	if v == "" {
		return netip.Addr{}, false
	}
	if i := strings.LastIndexByte(v, ','); i >= 0 {
		v = v[i+1:]
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(v))
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
