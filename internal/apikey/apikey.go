// Package apikey owns the single credential an outpost has: the key upcore
// presents on every request. The key is opaque, generated locally, and never
// leaves this process except through the startup banner and the key file.
package apikey

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// Prefix makes a key recognisable in a log or a paste buffer, the way
	// upcore's own keys are prefixed with upc_.
	Prefix = "opk_"

	// idBytes is a non-secret discriminator so a key can be talked about
	// (and grepped for) without revealing the secret half.
	idBytes = 4
	// secretBytes carries the entropy: 192 bits is far beyond what a probe
	// endpoint needs and still fits comfortably in a header.
	secretBytes = 24

	// FileName is the file inside the data dir that holds the key.
	FileName = "apikey"
)

var pattern = regexp.MustCompile(`^opk_[0-9a-f]{8}_[0-9a-f]{48}$`)

// Generate returns a fresh key. It fails only if the system entropy source
// does, which is not a condition worth continuing past.
func Generate() (string, error) {
	id := make([]byte, idBytes)
	if _, err := rand.Read(id); err != nil {
		return "", fmt.Errorf("read random id: %w", err)
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("read random secret: %w", err)
	}
	return Prefix + hex.EncodeToString(id) + "_" + hex.EncodeToString(secret), nil
}

// Valid reports whether token has the exact shape of an outpost key. It says
// nothing about whether the key is the configured one.
func Valid(token string) bool {
	return pattern.MatchString(token)
}

// Equal compares a presented key against the configured one in constant time,
// so the comparison's duration cannot be used to guess the key byte by byte.
// The length check is safe to do first: the length of a key is not a secret
// (it is fixed by the format), and ConstantTimeCompare requires equal lengths
// to be meaningful at all.
func Equal(presented, expected string) bool {
	if len(presented) != len(expected) || len(expected) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// Masked renders a key for logs: the public id survives, the secret does not.
func Masked(token string) string {
	if !Valid(token) {
		return "…"
	}
	return token[:len(Prefix)+idBytes*2] + "_…"
}

// Resolve establishes the key the server will accept, in the order an operator
// expects: an explicit override wins, a previously persisted key keeps the
// outpost's identity stable across restarts, and only a fresh install
// generates one.
//
// A data dir that cannot be written is not fatal — the outpost still serves,
// with a key that changes on every restart — because a probe with no history
// to lose is easier to re-enrol than to debug while it refuses to start.
//
// announce prints the key in a banner. An auto-enrolling outpost passes false:
// it sends the key to upcore itself, and telling the operator to copy something
// they never have to see is an invitation to leak it.
func Resolve(override, dataDir string, announce bool, log *slog.Logger) (string, error) {
	if override != "" {
		if !Valid(override) {
			return "", fmt.Errorf("OUTPOST_API_KEY is not a valid key (expected %s<8 hex>_<48 hex>)", Prefix)
		}
		log.Info("using the API key from OUTPOST_API_KEY", "key", Masked(override))
		return override, nil
	}

	path := filepath.Join(dataDir, FileName)
	if stored, err := read(path); err == nil {
		log.Info("using the stored API key", "path", path, "key", Masked(stored))
		return stored, nil
	} else if !os.IsNotExist(err) {
		log.Warn("cannot use the stored API key, generating a new one", "path", path, "error", err)
	}

	token, err := Generate()
	if err != nil {
		return "", err
	}

	if err := persist(path, token); err != nil {
		log.Warn("data dir is not writable: the API key is kept in memory only and WILL CHANGE on the next restart",
			"path", path, "error", err)
		if announce {
			banner(token, "")
		}
		return token, nil
	}

	log.Info("generated a new API key", "path", path, "key", Masked(token))
	if announce {
		banner(token, path)
	}
	return token, nil
}

func read(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if !Valid(token) {
		return "", fmt.Errorf("%s does not contain a valid key", path)
	}
	return token, nil
}

func persist(path, token string) error {
	// 0700/0600: on a bind-mounted host directory the file would otherwise be
	// world-readable, and this key is the only thing between the internet and
	// an arbitrary-target probe.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// banner prints the one and only plaintext rendering of a newly generated key.
// It goes to stderr directly rather than through slog because the text handler
// escapes newlines, which would collapse the box into one unreadable line —
// and the operator has to copy this value into upcore by hand.
func banner(token, path string) {
	const width = 70
	line := strings.Repeat("═", width)
	// Pad by runes rather than with fmt's %-*s: the arrows and dashes below are
	// multi-byte, and a byte-counted width would ragged the right border of the
	// one message the operator has to read carefully.
	row := func(s string) string {
		pad := width - 2 - utf8.RuneCountInString(s)
		if pad < 0 {
			pad = 0
		}
		return "║ " + s + strings.Repeat(" ", pad) + " ║\n"
	}

	var b strings.Builder
	b.WriteString("\n╔" + line + "╗\n")
	b.WriteString(row(""))
	b.WriteString(row("  outpost generated a new API key"))
	b.WriteString(row(""))
	b.WriteString(row("  " + token))
	b.WriteString(row(""))
	b.WriteString(row("  Copy it into upcore: Admin → Outposts → New."))
	if path != "" {
		b.WriteString(row("  Stored at " + path + " — it is not printed again."))
	} else {
		b.WriteString(row("  NOT stored: it changes on every restart."))
	}
	b.WriteString(row(""))
	b.WriteString("╚" + line + "╝\n\n")

	fmt.Fprint(os.Stderr, b.String())
}
