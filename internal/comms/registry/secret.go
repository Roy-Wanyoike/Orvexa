package registry

import (
	"encoding/json"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// redactRevealMin is the byte-length threshold below which a secret is
// masked without any suffix reveal: showing the last four bytes of an
// 8-byte value would disclose half of it. Values at or above the threshold
// reveal exactly their final four bytes for operator correlation.
const redactRevealMin = 12

// Redacted renders v so that zero usable credential material survives:
//   - "" stays "" (the field is unset and is omitted from renderings);
//   - values shorter than 12 bytes are fully masked as "****";
//   - longer values keep only their final four bytes ("****abcd");
//   - a suffix that would split a UTF-8 rune is masked entirely.
func Redacted(v string) string {
	switch {
	case v == "":
		return ""
	case len(v) < redactRevealMin:
		return "****"
	}
	if suffix := v[len(v)-4:]; utf8.ValidString(suffix) {
		return "****" + suffix
	}
	return "****"
}

// Secret is a credential value: a token, key, password, or identifier treated
// as sensitive (account SIDs, AMI usernames). Adapters intentionally retain
// direct access to the raw value (plain string conversion); every rendering
// path — fmt via Stringer/GoStringer, log/slog via LogValuer, encoding/json
// via Marshaler — exposes only Redacted output.
//
// Residual risk (accepted and documented in the package docs): fmt verbs
// outside v/s/q/x/X hit fmt's raw bad-verb path, and reflection or unsafe
// access reads the underlying string. Do not format Secret values with
// unusual verbs or route credential structs through reflection dumpers.
type Secret string

// Redacted implements the shared redaction helper.
func (s Secret) Redacted() string { return Redacted(string(s)) }

// String renders the masked form; never the raw value.
func (s Secret) String() string { return s.Redacted() }

// GoString renders the masked form so %#v cannot disclose the value.
func (s Secret) GoString() string { return s.Redacted() }

// LogValue renders the masked form for structured logging.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.Redacted()) }

// MarshalJSON renders the masked form.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.Redacted()) }

// kv is one rendered configuration field: a stable dotted key plus its
// already-redacted (or non-credential) value.
type kv struct {
	key string
	val string
}

// renderKV is the shared renderer for config Stringers:
// "telephony(provider=twilio twilio.auth_token=****abcd)".
func renderKV(kind string, fields []kv) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f.key+"="+f.val)
	}
	return kind + "(" + strings.Join(parts, " ") + ")"
}

// kvMap projects rendered fields into a JSON-safe map.
func kvMap(fields []kv) map[string]string {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f.key] = f.val
	}
	return m
}
