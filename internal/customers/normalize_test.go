package customers

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"+254712345678", "+254712345678"},
		{"254712345678", "+254712345678"},
		{"00 254 712 345678", "+254712345678"},
		{"0712345678", "+0712345678"}, // local formats pass through with '+', E.164 enforcement is an operator policy
	}
	for _, c := range cases {
		got, err := NormalizeIdentifier(IdentPhone, c.in)
		if err != nil {
			t.Errorf("phone %q rejected: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("phone %q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePhoneRejectsGarbage(t *testing.T) {
	for _, v := range []string{"abc", "12", "++12345678", "12345678901234567890"} {
		if _, err := NormalizeIdentifier(IdentPhone, v); err == nil {
			t.Errorf("phone %q must be rejected", v)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	got, err := NormalizeIdentifier(IdentEmail, "  Jane.Doe@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "jane.doe@example.com" {
		t.Fatalf("email not lowercased/trimmed: %q", got)
	}
	if _, err := NormalizeIdentifier(IdentEmail, "not-an-email"); err == nil {
		t.Fatal("invalid email must be rejected")
	}
}

func TestNormalizeWhatsAppSameAsPhone(t *testing.T) {
	p, _ := NormalizeIdentifier(IdentPhone, "+254712345678")
	w, _ := NormalizeIdentifier(IdentWhatsApp, "+254712345678")
	if p != w {
		t.Fatal("phone and whatsapp of the same number must normalize identically — otherwise channel identity forks")
	}
}

func TestNormalizeUnknownTypeRejected(t *testing.T) {
	if _, err := NormalizeIdentifier("carrier_pigeon", "x"); err == nil {
		t.Fatal("unknown identifier type must be rejected")
	}
}
