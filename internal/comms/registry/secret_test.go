package registry

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestRedacted(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},                    // unset fields are omitted by renderers
		{"ab", "****"},              // short values fully masked
		{"0123456789", "****"},      // 10 bytes
		{"01234567890", "****"},     // 11 bytes: still below threshold
		{"012345678901", "****901"}, // 12 bytes: threshold, last four reveal
		{strings.Repeat("a", 32), "****aaaa"},
		{strings.Repeat("α", 11), "****"}, // 22 bytes, but last four bytes split a rune
	}
	for _, tc := range cases {
		if got := Redacted(tc.in); got != tc.want {
			t.Errorf("Redacted(len=%d) = %q, want %q", len(tc.in), got, tc.want)
		}
	}
}

func TestSecretRenderersRevealNothing(t *testing.T) {
	const raw = "f4kesecret0000000zz99"
	s := Secret(raw)

	renderings := map[string]string{
		"String":  s.String(),
		"fmtV":    fmt.Sprintf("%v", s),
		"fmtS":    fmt.Sprintf("%s", s),
		"fmtQ":    fmt.Sprintf("%q", s),
		"fmtPlus": fmt.Sprintf("%+v", s),
		"fmtGo":   fmt.Sprintf("%#v", s),
		"json":    string(mustJSON(t, s)),
	}
	var jsonBuf, textBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Info("secret", "value", s)
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("secret", "value", s)
	renderings["slogJSON"] = jsonBuf.String()
	renderings["slogText"] = textBuf.String()

	for name, out := range renderings {
		if !strings.Contains(out, "****zz99") {
			t.Errorf("%s: expected masked last-four form, got %q", name, out)
		}
		if strings.Contains(out, "f4kesecret") {
			t.Errorf("%s: leaked the raw secret: %q", name, out)
		}
	}

	// Adapters intentionally retain direct access to the raw value.
	if string(s) != raw {
		t.Fatal("direct adapter access contract broken")
	}
	if s.Redacted() != "****zz99" {
		t.Errorf("Secret.Redacted() = %q, want ****zz99", s.Redacted())
	}
}
