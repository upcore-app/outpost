package apikey

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quiet keeps Resolve's bookkeeping out of the test output. The generated-key
// banner still reaches stderr, which is the behaviour an operator depends on.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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

func TestResolve(t *testing.T) {
	override, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	t.Run("override wins and is not persisted", func(t *testing.T) {
		dir := t.TempDir()
		got, err := Resolve(override, dir, quiet())
		if err != nil {
			t.Fatalf("Resolve() failed: %v", err)
		}
		if got != override {
			t.Errorf("Resolve() = %q, want the override %q", got, override)
		}
		// An override is the operator's own secret; writing a copy of it to a
		// mounted volume is not something they asked for.
		if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
			t.Errorf("Resolve() persisted the override: stat err = %v, want not-exist", err)
		}
	})

	t.Run("malformed override is fatal", func(t *testing.T) {
		if _, err := Resolve("nonsense", t.TempDir(), quiet()); err == nil {
			t.Error("Resolve() accepted a malformed OUTPOST_API_KEY, want an error")
		}
	})

	t.Run("generates once then reuses", func(t *testing.T) {
		dir := t.TempDir()
		first, err := Resolve("", dir, quiet())
		if err != nil {
			t.Fatalf("Resolve() failed: %v", err)
		}
		if !Valid(first) {
			t.Fatalf("Resolve() generated %q, which Valid() rejects", first)
		}

		info, err := os.Stat(filepath.Join(dir, FileName))
		if err != nil {
			t.Fatalf("the key was not persisted: %v", err)
		}
		// The file is the entire credential: on a bind-mounted data dir a laxer
		// mode would hand it to every user on the host.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file mode = %o, want 600", perm)
		}

		// A restart has to keep the outpost's identity, or upcore would need
		// re-enrolling every time the container is recreated.
		second, err := Resolve("", dir, quiet())
		if err != nil {
			t.Fatalf("Resolve() failed: %v", err)
		}
		if second != first {
			t.Errorf("Resolve() = %q on the second call, want the stored %q", second, first)
		}
	})

	t.Run("unreadable key file is replaced", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("garbage\n"), 0o600); err != nil {
			t.Fatalf("seeding the key file failed: %v", err)
		}
		got, err := Resolve("", dir, quiet())
		if err != nil {
			t.Fatalf("Resolve() failed: %v", err)
		}
		if !Valid(got) {
			t.Errorf("Resolve() = %q, want a freshly generated key", got)
		}
	})
}
