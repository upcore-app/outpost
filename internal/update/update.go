// Package update turns "upcore asked this outpost to upgrade itself" into
// something the host can actually carry out, and remembers what came of it.
//
// The hard part is not downloading a binary — install.sh already does that,
// with a checksum, and re-running it *is* the upgrade. The hard part is that
// the process which receives the request is the least privileged thing on the
// box: the systemd unit runs as an unprivileged user under ProtectSystem=strict
// with only its data directory writable, so it cannot replace /usr/local/bin/
// outpost, and it must not be able to. A probe that could rewrite its own
// binary would turn any bug in a check into persistence.
//
// So the outpost never updates itself. It asks:
//
//	systemd  it writes a request file into its data directory. A path unit
//	         installed beside the service notices, and a root-run oneshot
//	         re-runs the installer. Privilege stays where it was.
//	command  OUTPOST_UPDATE_COMMAND, run verbatim. The escape hatch for
//	         everything that is neither of the above — a Kubernetes rollout,
//	         a configuration-management run, a bespoke wrapper.
//	docker   a container cannot meaningfully replace its own image. Reported
//	         as such so upcore can say "pull a new image" instead of offering
//	         a button that would do nothing.
//
// What happens after a successful update is a restart, which means the process
// that accepted the request is not the process that reports the outcome. That
// is why the state lives in a file: an update is the one operation here whose
// result only exists on the other side of a restart.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Method is how this deployment can be upgraded, decided once at startup.
type Method string

const (
	// MethodNone: nothing is wired up. Either the operator turned it off, or
	// the outpost was installed in a way that has no upgrade path of its own.
	MethodNone Method = "none"
	// MethodSystemd: the path unit and the oneshot from install.sh are present.
	MethodSystemd Method = "systemd"
	// MethodCommand: OUTPOST_UPDATE_COMMAND is set and is run as given.
	MethodCommand Method = "command"
	// MethodDocker: a container. Reported, never applied.
	MethodDocker Method = "docker"
)

// The state a request goes through. Empty means nothing was ever asked for.
const (
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

const (
	// RequestName is the file the systemd path unit watches. Its content is the
	// requested version, or empty for "the latest release".
	RequestName = "update-request"
	// ResultName is how the privileged half reports back. Without it a failed
	// update — a checksum mismatch, a release with no binary for this
	// architecture — would be invisible until the request went stale twenty
	// minutes later, because a failure is exactly the case where no restart
	// happens and the version comparison has nothing to compare.
	//
	// Two lines: a verdict ("ok" or "failed") and a reason. Plain text rather
	// than JSON because it is written by a shell script, and a quoting mistake
	// there must not cost the message.
	ResultName = "update-result"

	// stateName holds the outcome across the restart the update causes.
	stateName = "update.json"

	// pathUnit is what install.sh writes when it installs the updater. Its
	// presence is the whole detection: if the privileged half is there, a
	// request can be carried out, and if it is not, nothing would read the file.
	pathUnit = "/etc/systemd/system/outpost-update.path"

	// dockerMarker is the conventional "this is a container" file. It is a
	// hint, not a guarantee — but a wrong answer here only changes which
	// sentence the dashboard shows.
	dockerMarker = "/.dockerenv"

	// commandTimeout bounds OUTPOST_UPDATE_COMMAND. Generous: the usual shape
	// is a download plus a service restart over a slow link.
	commandTimeout = 10 * time.Minute

	// staleAfter is how long a request may sit in "running" before it is called
	// a failure. An update that has not changed the version in this long did
	// not happen — the path unit is not enabled, the installer died, the image
	// was never pulled — and saying so beats a spinner that never stops.
	staleAfter = 20 * time.Minute

	// maxOutput is how much of a command's output is kept. The message is shown
	// in a table cell; the full output belongs in the host's own log.
	maxOutput = 2000
)

var (
	// ErrUnsupported: this deployment has no way to update itself.
	ErrUnsupported = errors.New("this outpost cannot update itself")
	// ErrBusy: an update is already under way. Asking twice would either run
	// two installers at once or, worse, overwrite the record of the first.
	ErrBusy = errors.New("an update is already running")
	// ErrBadVersion: the requested version is not a shape a release tag has.
	ErrBadVersion = errors.New("invalid version")
)

// versionPattern is deliberately narrow. The string reaches a root-run shell
// script as an argument, so what it may contain is decided here, once, rather
// than being quoted correctly in every place it is later used. Release tags are
// "v1.2.3" and at most "v1.2.3-rc.1"; nothing legitimate needs a slash, a
// space, or a dollar sign.
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// State is what upcore reads back on GET /v1/info. It is written to disk, so it
// survives the restart an update causes — which is the only way the outcome can
// be reported at all.
type State struct {
	// State is "", "running", "done" or "failed".
	State string `json:"state,omitempty"`
	// FromVersion is what was running when the request was accepted. It is what
	// makes "did this work?" answerable after the restart: a different version
	// now is the only proof the update landed.
	FromVersion string `json:"fromVersion,omitempty"`
	// TargetVersion is what was asked for; empty means "the latest release".
	TargetVersion string `json:"targetVersion,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	FinishedAt    string `json:"finishedAt,omitempty"`
	Message       string `json:"message,omitempty"`
}

// Manager owns the update state of one outpost process.
type Manager struct {
	method  Method
	command string
	dataDir string
	version string
	log     *slog.Logger

	mu    sync.Mutex
	state State
}

// Options is what the manager needs; it borrows nothing from the server so the
// two can be reasoned about separately.
type Options struct {
	// Enabled is OUTPOST_UPDATE_ENABLED. False forces MethodNone regardless of
	// what is installed on the host — an operator who manages the fleet from
	// somewhere else must be able to say so.
	Enabled bool
	// Command is OUTPOST_UPDATE_COMMAND, run through `sh -c` when set.
	Command string
	// DataDir holds the request and state files. Empty disables both, which
	// leaves the manager able to report a method but not to apply one.
	DataDir string
	// Version is what this binary reports as its own.
	Version string
}

// New detects how this deployment can be upgraded and reconciles the state of
// any update that was running when the process last stopped.
func New(opts Options, log *slog.Logger) *Manager {
	m := &Manager{
		command: strings.TrimSpace(opts.Command),
		dataDir: opts.DataDir,
		version: opts.Version,
		log:     log,
	}
	m.method = detect(opts.Enabled, m.command, m.dataDir)
	m.state = m.reconcile(m.load())
	return m
}

// detect decides the method once. The order is by specificity: an explicit
// command is a statement and wins over anything inferred from the host.
func detect(enabled bool, command, dataDir string) Method {
	if !enabled {
		return MethodNone
	}
	if command != "" {
		return MethodCommand
	}
	// Both halves are needed: the path unit reads the request, and the request
	// can only be written into a data directory that exists.
	if dataDir != "" && exists(pathUnit) {
		return MethodSystemd
	}
	if exists(dockerMarker) {
		return MethodDocker
	}
	return MethodNone
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Method reports how this outpost can be upgraded.
func (m *Manager) Method() Method { return m.method }

// CanApply reports whether Apply can do anything. Docker is a method upcore
// should name but never offer a button for, which is exactly this distinction.
func (m *Manager) CanApply() bool {
	return m.method == MethodSystemd || m.method == MethodCommand
}

// State returns a copy of the current state, after folding in anything the
// privileged updater left behind. Reading the result here rather than on a
// timer means it is picked up exactly when somebody asks — which is upcore
// polling GET /v1/info, the only reader there is.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.absorbResult()
	return m.state
}

// absorbResult reads and consumes the file the root-run updater writes. Called
// with m.mu held.
//
// An "ok" verdict does *not* end the request: the script exiting successfully
// says it ran, not that a new version is in place. The restart and the version
// comparison in reconcile are the only proof of that, and treating "ok" as done
// would report success for an installer that cheerfully reinstalled the same
// release.
func (m *Manager) absorbResult() {
	if m.dataDir == "" {
		return
	}
	path := filepath.Join(m.dataDir, ResultName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Consumed even when it is unreadable or arrives with nothing running: a
	// result nobody can act on must not be read again on every request.
	_ = os.Remove(path)

	verdict, reason, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	verdict = strings.TrimSpace(verdict)
	reason = tail(reason)

	if m.state.State != StateRunning {
		return
	}
	if verdict == "ok" {
		if reason != "" {
			m.state.Message = reason
			m.persist(m.state)
		}
		return
	}

	m.state.State = StateFailed
	m.state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if reason == "" {
		reason = "Das Update ist auf dem Host fehlgeschlagen (journalctl -u outpost-update)."
	}
	m.state.Message = reason
	m.persist(m.state)
	m.log.Error("update failed on the host", "message", reason)
}

// Apply accepts an update request and returns the state it produced. It does
// not wait for the update: on a systemd host the work happens in another unit,
// and on any host a successful update ends this process. The caller answers
// 202 and upcore learns the outcome from the next GET /v1/info.
func (m *Manager) Apply(version string) (State, error) {
	version = strings.TrimSpace(version)
	if version != "" && !versionPattern.MatchString(version) {
		return m.State(), ErrBadVersion
	}
	if !m.CanApply() {
		return m.State(), ErrUnsupported
	}

	m.mu.Lock()
	// Before the busy check, not after: an update that already failed on the
	// host must not block the retry for the twenty minutes it takes to go stale.
	m.absorbResult()
	if m.state.State == StateRunning && !m.stale(m.state) {
		state := m.state
		m.mu.Unlock()
		return state, ErrBusy
	}
	now := time.Now().UTC()
	m.state = State{
		State:         StateRunning,
		FromVersion:   m.version,
		TargetVersion: version,
		StartedAt:     now.Format(time.RFC3339),
	}
	state := m.state
	m.mu.Unlock()

	// Persisted before the trigger, not after: the request may end this process
	// within milliseconds, and a state file written afterwards would sometimes
	// never be written at all.
	m.persist(state)

	var err error
	switch m.method {
	case MethodSystemd:
		err = m.requestViaSystemd(version)
	case MethodCommand:
		go m.runCommand(version)
	default:
		err = ErrUnsupported
	}

	if err != nil {
		state = m.finish(StateFailed, err.Error())
		return state, err
	}

	m.log.Info("update requested", "method", string(m.method), "target", versionLabel(version))
	return state, nil
}

// requestViaSystemd drops the file the path unit watches. Writing it is the
// entire trigger: systemd starts outpost-update.service, which re-runs the
// installer as root and restarts this service underneath us.
//
// Written to a temporary name first and renamed into place, because the path
// unit fires on the file appearing — a partially written request would be read
// by a root-run script.
func (m *Manager) requestViaSystemd(version string) error {
	if m.dataDir == "" {
		return errors.New("no data directory to write the update request to")
	}
	target := filepath.Join(m.dataDir, RequestName)
	tmp := target + ".tmp"

	// 0600: the file is only ever read by root, and its content decides which
	// release a privileged script downloads.
	if err := os.WriteFile(tmp, []byte(version+"\n"), 0o600); err != nil {
		return fmt.Errorf("write the update request: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("place the update request: %w", err)
	}
	return nil
}

// runCommand runs OUTPOST_UPDATE_COMMAND and records what came of it.
//
// Whether this goroutine survives to write the result depends on what the
// command does: one that restarts this service kills its own caller, because
// systemd stops the whole control group. That is fine and expected — the
// reconcile on the next start turns the abandoned "running" into a verdict. The
// result is recorded here for the commands that do *not* end the process
// (a rollout that is merely triggered, a script that schedules the work).
func (m *Manager) runCommand(version string) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", m.command)
	cmd.Env = append(os.Environ(),
		"OUTPOST_TARGET_VERSION="+version,
		"OUTPOST_CURRENT_VERSION="+m.version,
	)
	output, err := cmd.CombinedOutput()
	text := tail(string(output))

	if err != nil {
		m.log.Error("update command failed", "error", err, "output", text)
		m.finish(StateFailed, strings.TrimSpace(err.Error()+": "+text))
		return
	}
	m.log.Info("update command finished", "output", text)
	// Deliberately not StateDone: the command exiting 0 says it ran, not that a
	// new version is in place. The next start compares the versions and has the
	// only answer worth reporting.
	m.finish(StateRunning, text)
}

// finish records a terminal (or updated) state and persists it.
func (m *Manager) finish(state, message string) State {
	m.mu.Lock()
	m.state.State = state
	m.state.Message = tail(message)
	if state != StateRunning {
		m.state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}
	updated := m.state
	m.mu.Unlock()

	m.persist(updated)
	return updated
}

// reconcile decides what an update that was running at the last shutdown
// actually did. This runs at startup, which is the only vantage point from
// which the question can be answered: the process that accepted the request is
// gone, and the version this binary reports is the evidence.
func (m *Manager) reconcile(state State) State {
	if state.State != StateRunning {
		return state
	}

	finished := time.Now().UTC().Format(time.RFC3339)

	// A different version is running than the one that asked. That is the
	// update, and it is the only proof of it there is.
	if state.FromVersion != "" && state.FromVersion != m.version {
		state.FinishedAt = finished
		if state.TargetVersion != "" && state.TargetVersion != m.version &&
			strings.TrimPrefix(state.TargetVersion, "v") != strings.TrimPrefix(m.version, "v") {
			// It moved, but not to what was asked for. Worth saying: the usual
			// cause is a tag that has no binary for this architecture and an
			// installer that fell back to the latest release.
			state.State = StateDone
			state.Message = fmt.Sprintf("Aktualisiert von %s auf %s (angefordert war %s).",
				state.FromVersion, m.version, state.TargetVersion)
			return state
		}
		state.State = StateDone
		state.Message = fmt.Sprintf("Aktualisiert von %s auf %s.", state.FromVersion, m.version)
		return state
	}

	// Same version, and long enough ago that nothing is still working on it.
	if m.stale(state) {
		state.State = StateFailed
		state.FinishedAt = finished
		state.Message = "Das Update wurde angestoßen, die Version hat sich aber nicht geändert. " +
			"Läuft der Updater auf dem Host (journalctl -u outpost-update)?"
		return state
	}

	// Same version, recently: this is a restart that had nothing to do with the
	// update, or the update is genuinely still running. Leave it alone.
	return state
}

// stale reports whether a running request has been running too long to still be
// believed. A request with no readable start time is treated as stale: an
// unparseable timestamp must not be able to wedge the state at "running".
func (m *Manager) stale(state State) bool {
	started, err := time.Parse(time.RFC3339, state.StartedAt)
	if err != nil {
		return true
	}
	return time.Since(started) > staleAfter
}

// load reads the state file. Anything unreadable is treated as "no state": this
// is bookkeeping, and a corrupt file must not stop a probe from booting.
func (m *Manager) load() State {
	if m.dataDir == "" {
		return State{}
	}
	raw, err := os.ReadFile(filepath.Join(m.dataDir, stateName))
	if err != nil {
		return State{}
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		m.log.Warn("ignoring an unreadable update state file", "error", err)
		return State{}
	}
	return state
}

// persist writes the state file, best effort. A failed write costs the outcome
// report after the next restart and nothing else, so it is logged rather than
// returned.
func (m *Manager) persist(state State) {
	if m.dataDir == "" {
		return
	}
	raw, err := json.Marshal(state)
	if err != nil {
		m.log.Error("cannot encode the update state", "error", err)
		return
	}
	path := filepath.Join(m.dataDir, stateName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		m.log.Error("cannot write the update state", "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		m.log.Error("cannot place the update state", "error", err)
	}
}

// Hint is the sentence upcore shows when there is no button to offer. It names
// the one command that upgrades this particular deployment, because "update the
// outpost" is a different operation on each of them.
func (m *Manager) Hint() string {
	switch m.method {
	case MethodDocker:
		return "Container-Deployment: docker compose pull && docker compose up -d " +
			"(oder das Image neu ziehen und den Container ersetzen)."
	case MethodSystemd, MethodCommand:
		return ""
	default:
		if exists("/run/systemd/system") {
			return "Kein Updater installiert. install.sh erneut ausführen richtet ihn ein — " +
				"dasselbe Kommando aktualisiert den Outpost auch direkt."
		}
		return "Für dieses Deployment ist kein automatisches Update eingerichtet."
	}
}

func versionLabel(version string) string {
	if version == "" {
		return "latest"
	}
	return version
}

// tail keeps the end of a long output: a failure's reason is at the bottom, not
// at the top.
func tail(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxOutput {
		return text
	}
	return "…" + text[len(text)-maxOutput:]
}
