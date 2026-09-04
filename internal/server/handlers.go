package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/upcore-app/outpost/internal/check"
	"github.com/upcore-app/outpost/internal/update"
)

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type infoResponse struct {
	Version        string   `json:"version"`
	Location       string   `json:"location"`
	Provider       string   `json:"provider"`
	Country        string   `json:"country"`
	CheckTypes     []string `json:"checkTypes"`
	MaxConcurrency int      `json:"maxConcurrency"`
	MaxChecks      int      `json:"maxChecks"`
	StartedAt      string   `json:"startedAt"`
	UptimeSeconds  int      `json:"uptimeSeconds"`

	// How this deployment can be upgraded, whether upcore may offer a button for
	// it, and what came of the last attempt. `canUpdate` is not implied by
	// `updateMethod`: a container reports "docker" and cannot be updated from
	// here, which is a different sentence from "nothing is wired up".
	UpdateMethod string       `json:"updateMethod"`
	CanUpdate    bool         `json:"canUpdate"`
	UpdateHint   string       `json:"updateHint,omitempty"`
	Update       update.State `json:"update"`
}

// updateRequest is the body of POST /v1/update. An empty version means "the
// latest release"; internal/update decides what shape a tag may have, because
// the string reaches a privileged script as an argument.
type updateRequest struct {
	Version string `json:"version"`
}

type updateResponse struct {
	Accepted bool         `json:"accepted"`
	Method   string       `json:"method"`
	Update   update.State `json:"update"`
}

type checksRequest struct {
	Checks []check.Check `json:"checks"`
}

type checksResponse struct {
	Results []check.Result `json:"results"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// handleHealth is deliberately unauthenticated: it is what a container
// healthcheck and a load balancer poll, and it reveals nothing but liveness.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: s.version})
}

// handleInfo is how upcore verifies a key and prefills an outpost's location in
// the admin UI, so it answers with the operator's own configuration verbatim.
func (s *server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, infoResponse{
		Version:        s.version,
		Location:       s.cfg.Location,
		Provider:       s.cfg.Provider,
		Country:        s.cfg.Country,
		CheckTypes:     check.Types(),
		MaxConcurrency: s.cfg.MaxConcurrency,
		MaxChecks:      s.cfg.MaxChecks,
		StartedAt:      s.startedAt,
		UptimeSeconds:  int(time.Since(s.started).Seconds()),
		UpdateMethod:   string(s.updater.Method()),
		CanUpdate:      s.updater.CanApply(),
		UpdateHint:     s.updater.Hint(),
		Update:         s.updater.State(),
	})
}

// handleUpdate accepts an upgrade request from upcore and answers 202: on every
// method that works, the outpost is restarted out from under this response, so
// there is nothing to wait for and nothing useful to say afterwards. upcore
// learns the outcome from the next GET /v1/info, which reads the state file the
// update wrote before it began (see internal/update).
//
// An empty body is legal and means "the latest release" — that is the request
// the dashboard's button sends, and requiring a version there would mean upcore
// had to know the release list before it could ask for anything.
func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateBodyBytes)

	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	state, err := s.updater.Apply(req.Version)
	switch {
	case errors.Is(err, update.ErrBadVersion):
		writeError(w, http.StatusBadRequest, "not a release tag: "+req.Version)
		return
	case errors.Is(err, update.ErrBusy):
		// 409 rather than 202: the caller asked for a second update, and got
		// neither a new one nor a failure. The state says which one is running.
		writeJSON(w, http.StatusConflict, updateResponse{
			Accepted: false, Method: string(s.updater.Method()), Update: state,
		})
		return
	case errors.Is(err, update.ErrUnsupported):
		// 501, because the request is well formed and this host simply has no
		// way to carry it out. The hint on /v1/info says what would.
		writeJSON(w, http.StatusNotImplemented, updateResponse{
			Accepted: false, Method: string(s.updater.Method()), Update: state,
		})
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "cannot start the update: "+err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, updateResponse{
		Accepted: true, Method: string(s.updater.Method()), Update: state,
	})
}

func (s *server) handleChecks(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req checksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if err := s.validate(req.Checks); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The batch inherits the client's context — a disconnected upcore stops the
	// probes — plus a bound of its own so a hung probe cannot outlive the
	// response the caller is waiting for.
	ctx, cancel := context.WithTimeout(r.Context(), maxBatchDuration)
	defer cancel()

	results := s.runner.Run(ctx, req.Checks)
	writeJSON(w, http.StatusOK, checksResponse{Results: results})
}

// validate rejects only what makes a result unattributable or the batch
// unbounded. Everything else — an unknown type, an empty target — is reported
// per check, because one bad descriptor must not cost upcore the whole batch.
func (s *server) validate(checks []check.Check) error {
	if len(checks) == 0 {
		return fmt.Errorf("checks must contain at least one check")
	}
	if len(checks) > s.cfg.MaxChecks {
		return fmt.Errorf("too many checks: %d requested, %d is the maximum", len(checks), s.cfg.MaxChecks)
	}

	seen := make(map[string]struct{}, len(checks))
	for i, c := range checks {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("checks[%d]: id is required", i)
		}
		if len(c.ID) > maxIDLength {
			return fmt.Errorf("checks[%d]: id must be at most %d characters", i, maxIDLength)
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("checks[%d]: duplicate id", i)
		}
		seen[c.ID] = struct{}{}
	}
	return nil
}

func handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
}

func methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, http.StatusMethodNotAllowed, r.Method+" is not allowed here, use "+allowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already out, so a failed encode can only be logged by
	// the caller's transport; there is nothing useful left to say to the client.
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
