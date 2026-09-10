# internal/webhooks/testdata — golden vectors & attack fixtures (issue #24)

`testdata/` is ignored by the Go toolchain; these files are loaded by the
tests in `verify_test.go` only.

## Files

| File | Purpose |
|---|---|
| `twilio_golden.json` | Captured-spec golden vectors for `X-Twilio-Signature` (form-encoded request-validator scheme), incl. unicode sort-order, URL-with-query-string, percent/plus decoding, rotated-token and duplicate-key vectors. |
| `whatsappcloud_golden.json` | Captured-spec golden vectors for `X-Hub-Signature-256` (`sha256=` + hex HMAC-SHA256 over exact raw body bytes), incl. unicode body and trailing-newline byte-binding vectors. |
| `africastalking_allowlist.json` | Decision-matrix fixture for Africa's Talking: AT documents NO signature/checksum surface, so the fixture is the peer-IP allowlist behavior table. |
| `attack_matrix.json` | Adversarial matrix (tamper / replay / wrong-credential / missing-header / trailing-newline / empty-secret fail-closed / peer-identity attacks) — executed case-by-case by `TestAttackMatrix`. |
| `generate_golden.py` | Deterministic generator for the three fixture files. |

## Provenance & integrity of the goldens

The `*_golden.json` signatures are produced by `generate_golden.py` using
**Python 3.12 stdlib `hmac`/`hashlib`** — an implementation completely
independent of the Go verifiers under test. The Go tests therefore prove
cross-implementation agreement with the captured provider spec (and with the
canonical vendor validators), not self-consistency. Regenerate with:

    python3 testdata/generate_golden.py

Output is deterministic (fixed synthetic credentials, fixed bodies).

## Credentials are synthetic and clearly fake

Every token/secret in these fixtures is an obviously-fake constant
(`c0ffee00deadbeef…`, `feedface00000000…`, `a1b2c3d4e5f6…`); every URL uses
the reserved `.example` TLD (`webhook.orvexa.example`); CIDRs are
documentation-range examples, **not** Africa's Talking's published egress
ranges. Nothing in this directory can authenticate against any real carrier.
If a real credential ever leaks into a fixture, treat it as compromised:
rotate it at the provider AND regenerate the fixtures with synthetic values.
