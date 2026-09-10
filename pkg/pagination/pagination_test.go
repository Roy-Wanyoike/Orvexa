package pagination

import (
	"testing"
)

func TestParseDefaults(t *testing.T) {
	p, err := Parse("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != DefaultLimit || p.Offset != 0 {
		t.Fatalf("defaults wrong: %+v", p)
	}
}

func TestParseClampsOversizedLimit(t *testing.T) {
	p, err := Parse("", 100000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != MaxLimit {
		t.Fatalf("limit not clamped: %d", p.Limit)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	first, _ := Parse("", 25)
	cur := first.NextCursor(25) // full page → cursor
	if cur == "" {
		t.Fatal("full page must produce a cursor")
	}
	second, err := Parse(cur, 25)
	if err != nil {
		t.Fatal(err)
	}
	if second.Offset != 25 || second.Limit != 25 {
		t.Fatalf("cursor decode wrong: %+v", second)
	}
	if second.NextCursor(10) != "" {
		t.Fatal("short page must NOT produce a cursor")
	}
}

func TestMalformedCursorsRejected(t *testing.T) {
	for _, bad := range []string{"!!!", "aGVsbG8", "MTIzNDU", "0|abc"} {
		if _, err := Parse(bad, 10); err == nil {
			t.Fatalf("cursor %q must be rejected", bad)
		}
	}
}
