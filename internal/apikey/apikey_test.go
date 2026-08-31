package apikey

import (
	"strings"
	"testing"
)

func TestGenerateRoundTrip(t *testing.T) {
	token, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}
	if !Valid(token) {
		t.Fatalf("Generate() produced %q, which Valid() rejects", token)
	}
	if want := len(Prefix) + 8 + 1 + 48; len(token) != want {
		t.Errorf("len(token) = %d, want %d", len(token), want)
	}
	if !Equal(token, token) {
		t.Error("Equal() rejected a key compared with itself")
	}
	if masked := Masked(token); masked != token[:12]+"_…" {
		t.Errorf("Masked() = %q, want %q", masked, token[:12]+"_…")
	}
	if strings.Contains(Masked(token), token[13:]) {
		t.Error("Masked() leaked the secret half of the key")
	}

	other, err := Generate()
	if err != nil {
		t.Fatalf("second Generate() failed: %v", err)
	}
	if other == token {
		t.Error("Generate() returned the same key twice")
	}
	if Equal(other, token) {
		t.Error("Equal() accepted two different keys")
	}
}

func TestValid(t *testing.T) {
	const good = "opk_0123abcd_0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"well formed", good, true},
		{"empty", "", false},
		{"wrong prefix", "upc" + good[3:], false},
		{"uppercase hex", strings.ToUpper(good), false},
		{"id too short", "opk_0123abc_0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"secret too short", "opk_0123abcd_0123456789abcdef", false},
		{"trailing junk", good + "x", false},
		{"leading whitespace", " " + good, false},
		{"non hex", "opk_0123abcz_0123456789abcdef0123456789abcdef0123456789abcdef", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.token); got != tc.want {
				t.Errorf("Valid(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

func TestEqual(t *testing.T) {
	const expected = "opk_0123abcd_0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name      string
		presented string
		want      bool
	}{
		{"exact", expected, true},
		{"last character flipped", expected[:len(expected)-1] + "0", false},
		{"first hex digit flipped", "opk_1123abcd_0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"prefix only", "opk_0123abcd_", false},
		{"one character longer", expected + "f", false},
		{"empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Equal(tc.presented, expected); got != tc.want {
				t.Errorf("Equal(%q, expected) = %v, want %v", tc.presented, got, tc.want)
			}
		})
	}

	if Equal("", "") {
		t.Error("Equal() accepted two empty strings; an unconfigured key must never match")
	}
}

func TestMaskedRejectsMalformedInput(t *testing.T) {
	for _, token := range []string{"", "nonsense", "opk_short"} {
		if got := Masked(token); got != "…" {
			t.Errorf("Masked(%q) = %q, want %q", token, got, "…")
		}
	}
}
