// Package config reads the outpost's runtime settings from the environment.
// There is no config file: an outpost is a disposable container, and everything
// but the API key is stateless.
package config

import (
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Config is the fully resolved, already clamped configuration. Nothing else in
// the process re-reads the environment.
type Config struct {
	Addr           string
	Location       string
	Provider       string
	Country        string
	DataDir        string
	APIKey         string
	MaxConcurrency int
	MaxChecks      int
	LogRequests    bool

	// Auto-setup. All three are empty in a hand-registered deployment; the two
	// below go together, and internal/enroll says so when only one is set.
	//
	// UpcoreURL and SetupKey come from the deploy command upcore generated;
	// PublicURL is only needed where the address upcore sees is not the address
	// it can call back — behind a proxy, a NAT, or in Kubernetes.
	UpcoreURL string
	SetupKey  string
	PublicURL string
}

const (
	defaultAddr           = ":8080"
	defaultDataDir        = "/data"
	defaultMaxConcurrency = 50
	defaultMaxChecks      = 200

	minConcurrency = 1
	maxConcurrency = 500
	minChecks      = 1
	maxChecks      = 1000
)

// countryPattern is ISO 3166-1 alpha-2: upcore renders it as a flag, so a
// three-letter code or a country name would break its UI rather than this one.
var countryPattern = regexp.MustCompile(`^[A-Za-z]{2}$`)

// Load resolves the configuration, logging a warning for every value it had to
// correct. A bad value never stops the process: an outpost that refuses to boot
// is worse for the operator than one running with a clamped default.
func Load(log *slog.Logger) Config {
	cfg := Config{
		Addr:           env("OUTPOST_ADDR", defaultAddr),
		Location:       env("OUTPOST_LOCATION", ""),
		Provider:       env("OUTPOST_PROVIDER", ""),
		DataDir:        env("OUTPOST_DATA_DIR", defaultDataDir),
		APIKey:         env("OUTPOST_API_KEY", ""),
		MaxConcurrency: envInt(log, "OUTPOST_MAX_CONCURRENCY", defaultMaxConcurrency, minConcurrency, maxConcurrency),
		MaxChecks:      envInt(log, "OUTPOST_MAX_CHECKS", defaultMaxChecks, minChecks, maxChecks),
		LogRequests:    envBool(log, "OUTPOST_LOG_REQUESTS", true),
		UpcoreURL:      strings.TrimRight(env("OUTPOST_UPCORE_URL", ""), "/"),
		SetupKey:       env("OUTPOST_SETUP_KEY", ""),
		PublicURL:      strings.TrimRight(env("OUTPOST_PUBLIC_URL", ""), "/"),
	}

	if country := env("OUTPOST_COUNTRY", ""); country != "" {
		if countryPattern.MatchString(country) {
			cfg.Country = strings.ToUpper(country)
		} else {
			log.Warn("ignoring OUTPOST_COUNTRY: expected an ISO 3166-1 alpha-2 code", "value", country)
		}
	}

	return cfg
}

// env reads a variable and trims it: values pasted into a compose file or a
// systemd unit routinely carry trailing whitespace.
func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envInt(log *slog.Logger, name string, fallback, min, max int) int {
	raw := env(name, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Warn("ignoring non-numeric value", "var", name, "value", raw, "using", fallback)
		return fallback
	}
	if n < min {
		log.Warn("value below the supported minimum, clamping", "var", name, "value", n, "using", min)
		return min
	}
	if n > max {
		log.Warn("value above the supported maximum, clamping", "var", name, "value", n, "using", max)
		return max
	}
	return n
}

func envBool(log *slog.Logger, name string, fallback bool) bool {
	raw := env(name, "")
	if raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		log.Warn("ignoring non-boolean value", "var", name, "value", raw, "using", fallback)
		return fallback
	}
	return b
}
