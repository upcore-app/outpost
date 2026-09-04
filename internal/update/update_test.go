package update

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// withState writes a state file into a fresh data dir and returns the dir.
func withState(t *testing.T, state State) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateName), raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return dir
}

func manager(t *testing.T, dir, version, command string) *Manager {
	t.Helper()
	return New(Options{Enabled: true, Command: command, DataDir: dir, Version: version}, quiet())
}

func TestDetectPrefersAnExplicitCommand(t *testing.T) {
	m := manager(t, t.TempDir(), "1.0.0", "echo hi")
	if m.Method() != MethodCommand {
		t.Fatalf("method = %q, want %q", m.Method(), MethodCommand)
	}
	if !m.CanApply() {
		t.Fatal("a configured command must be applicable")
	}
}

func TestDisabledBeatsEverything(t *testing.T) {
	m := New(Options{Enabled: false, Command: "echo hi", DataDir: t.TempDir(), Version: "1.0.0"}, quiet())
	if m.Method() != MethodNone {
		t.Fatalf("method = %q, want %q", m.Method(), MethodNone)
	}
	if _, err := m.Apply(""); err != ErrUnsupported {
		t.Fatalf("Apply on a disabled updater = %v, want %v", err, ErrUnsupported)
	}
}

// The version reaches a root-run script as an argument, so the shapes it may
// have are the security boundary and not a nicety.
func TestApplyRejectsAVersionThatIsNotATag(t *testing.T) {
	m := manager(t, t.TempDir(), "1.0.0", "true")
	for _, bad := range []string{
		"v1.0.0; rm -rf /",
		"$(id)",
		"`id`",
		"../../etc/passwd",
		"v1 0",
		"-rf",
		strings.Repeat("v", 200),
	} {
		if _, err := m.Apply(bad); err != ErrBadVersion {
			t.Errorf("Apply(%q) = %v, want %v", bad, err, ErrBadVersion)
		}
	}

	for _, good := range []string{"", "v1.2.3", "1.2.3", "v1.2.3-rc.1", "v2.0.0+build.5"} {
		if _, err := m.Apply(good); err == ErrBadVersion {
			t.Errorf("Apply(%q) rejected a legal tag", good)
		}
		// Each accepted request leaves the manager running; clear it so the next
		// one is not refused as a duplicate.
		m.state = State{}
	}
}

func TestApplyRefusesASecondRequest(t *testing.T) {
	m := manager(t, t.TempDir(), "1.0.0", "sleep 5")
	if _, err := m.Apply("v1.1.0"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := m.Apply("v1.2.0"); err != ErrBusy {
		t.Fatalf("second Apply = %v, want %v", err, ErrBusy)
	}
}

// A request that has been "running" longer than anything plausibly takes is not
// a lock on the endpoint forever.
func TestApplyAcceptsAgainOnceTheRequestIsStale(t *testing.T) {
	dir := withState(t, State{
		State:       StateRunning,
		FromVersion: "1.0.0",
		StartedAt:   time.Now().Add(-2 * staleAfter).UTC().Format(time.RFC3339),
	})
	m := manager(t, dir, "1.0.0", "true")

	// The constructor already turned the stale request into a failure.
	if got := m.State().State; got != StateFailed {
		t.Fatalf("reconciled state = %q, want %q", got, StateFailed)
	}
	if _, err := m.Apply("v1.1.0"); err != nil {
		t.Fatalf("Apply after a stale request: %v", err)
	}
}

// The whole point of the state file: the process that accepted the request is
// not the process that reports the outcome.
func TestReconcileCallsAVersionChangeSuccess(t *testing.T) {
	dir := withState(t, State{
		State:         StateRunning,
		FromVersion:   "1.0.0",
		TargetVersion: "v1.1.0",
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	})
	m := manager(t, dir, "1.1.0", "true")

	state := m.State()
	if state.State != StateDone {
		t.Fatalf("state = %q, want %q", state.State, StateDone)
	}
	if !strings.Contains(state.Message, "1.1.0") {
		t.Errorf("message %q does not name the new version", state.Message)
	}
	if state.FinishedAt == "" {
		t.Error("a finished update has to carry a finishing time")
	}
}

// "v1.1.0" and "1.1.0" are the same release: the tag carries the v, the binary
// is stamped with whatever the build passed.
func TestReconcileIgnoresTheTagPrefix(t *testing.T) {
	dir := withState(t, State{
		State:         StateRunning,
		FromVersion:   "1.0.0",
		TargetVersion: "v1.1.0",
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	})
	m := manager(t, dir, "1.1.0", "true")
	if msg := m.State().Message; strings.Contains(msg, "angefordert") {
		t.Errorf("message %q treats v1.1.0 and 1.1.0 as different releases", msg)
	}
}

func TestReconcileReportsAVersionOtherThanTheOneAskedFor(t *testing.T) {
	dir := withState(t, State{
		State:         StateRunning,
		FromVersion:   "1.0.0",
		TargetVersion: "v1.5.0",
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	})
	m := manager(t, dir, "1.1.0", "true")

	state := m.State()
	if state.State != StateDone {
		t.Fatalf("state = %q, want %q", state.State, StateDone)
	}
	if !strings.Contains(state.Message, "v1.5.0") {
		t.Errorf("message %q does not say what was actually asked for", state.Message)
	}
}

// A restart that had nothing to do with the update must not be read as one.
func TestReconcileLeavesARecentRequestAlone(t *testing.T) {
	dir := withState(t, State{
		State:       StateRunning,
		FromVersion: "1.0.0",
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	})
	m := manager(t, dir, "1.0.0", "true")
	if got := m.State().State; got != StateRunning {
		t.Fatalf("state = %q, want %q", got, StateRunning)
	}
}

// An unparseable start time must not be able to wedge the state at "running".
func TestReconcileTreatsAnUnreadableStartTimeAsStale(t *testing.T) {
	dir := withState(t, State{State: StateRunning, FromVersion: "1.0.0", StartedAt: "whenever"})
	m := manager(t, dir, "1.0.0", "true")
	if got := m.State().State; got != StateFailed {
		t.Fatalf("state = %q, want %q", got, StateFailed)
	}
}

// Bookkeeping must not be able to stop a probe from booting.
func TestACorruptStateFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := manager(t, dir, "1.0.0", "true")
	if got := m.State().State; got != "" {
		t.Fatalf("state = %q, want empty", got)
	}
}

// The systemd path is the one that matters most: the file appearing *is* the
// trigger for a root-run script, so it has to appear complete or not at all.
func TestSystemdRequestWritesTheTagAtomically(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{method: MethodSystemd, dataDir: dir, version: "1.0.0", log: quiet()}

	if _, err := m.Apply("v1.4.2"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, RequestName))
	if err != nil {
		t.Fatalf("read the request: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "v1.4.2" {
		t.Errorf("request = %q, want v1.4.2", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, RequestName+".tmp")); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}

	// The state has to be on disk before the trigger: the restart the updater
	// causes can arrive before anything else is written.
	if _, err := os.Stat(filepath.Join(dir, stateName)); err != nil {
		t.Errorf("no state file after Apply: %v", err)
	}
}

func TestSystemdRequestIsEmptyForTheLatestRelease(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{method: MethodSystemd, dataDir: dir, version: "1.0.0", log: quiet()}
	if _, err := m.Apply(""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, RequestName))
	if err != nil {
		t.Fatalf("read the request: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "" {
		t.Errorf("request = %q, want an empty tag", raw)
	}
}

// Docker is a method upcore should name, never a button it should offer.
func TestDockerIsReportedButNotApplicable(t *testing.T) {
	m := &Manager{method: MethodDocker, dataDir: t.TempDir(), version: "1.0.0", log: quiet()}
	if m.CanApply() {
		t.Fatal("a container must not claim it can update itself")
	}
	if _, err := m.Apply(""); err != ErrUnsupported {
		t.Fatalf("Apply = %v, want %v", err, ErrUnsupported)
	}
	if m.Hint() == "" {
		t.Error("a method with no button has to say what to run instead")
	}
}
