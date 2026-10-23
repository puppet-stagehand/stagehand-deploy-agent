package server

// Stagehand: the /api/v1/deploys route group (D-10, D-12), ported from the
// fork's api/deploys.go with gin replaced by net/http. Every status, header and
// string is the fork's, byte for byte, except the differences listed in
// contract.Divergences (an oversized body answers 413 and data after the JSON
// object answers 400; ServeMux answers unknown paths). contract.TestGolden
// holds this file to that. Callers have already presented an allow-listed
// Puppet certificate in the TLS handshake; nothing here authorises anyone.
//
//	POST /api/v1/deploys       {"environments":[{"name":"production","expected_sha":"<40 hex>"}],"wait":false}
//	                           wait false: 202 with the queued record; poll GET by id (MH-01)
//	                           wait true:  200 with the finished record
//	GET  /api/v1/deploys/{id}  the job record
//	GET  /api/v1/deploys?limit=N  {"deploys":[record, ...]} newest first, N in 1..100 (default 20)
//
// Records are files under deploys.state_dir/jobs (jobs.FileStore), so they
// survive a restart.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

const (
	maxDeployBody    = 64 << 10
	defaultListLimit = 20
	maxListLimit     = jobs.MaxListLimit

	// Request hardening, the two deliberate request-side differences from the
	// fork (FORK-IN-06; contract.Divergences). TestDeployOversizedBody ties the
	// number in bodyTooLarge to maxDeployBody.
	bodyTooLarge = "request body exceeds 65536 bytes"
	trailingData = "request body is not valid JSON: unexpected data after the JSON object"
)

// DeployHandlers serves the deploy API.
type DeployHandlers struct {
	runner *jobs.Runner
	log    *slog.Logger
}

// NewDeployHandlers builds the handlers around runner. A nil log means
// slog.Default().
func NewDeployHandlers(runner *jobs.Runner, log *slog.Logger) *DeployHandlers {
	if log == nil {
		log = slog.Default()
	}
	return &DeployHandlers{runner: runner, log: log.With("component", "deploys")}
}

type deployRequest struct {
	Environments []jobs.EnvRequest `json:"environments"`
	// Wait is kept raw so that a missing or non-boolean value answers 422 on
	// the field rather than 400.
	Wait json.RawMessage `json:"wait"`
}

// wait reports the request's wait flag; validate has checked it.
func (req deployRequest) wait() bool { return string(req.Wait) == "true" }

func unprocessable(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": field + ": " + msg, "field": field})
}

func failure(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// validate returns the first invalid field and why, or "" when req is valid.
func (req deployRequest) validate() (field, msg string) {
	if n := len(req.Environments); n == 0 || n > jobs.MaxEnvironments {
		return "environments", fmt.Sprintf("must name 1 to %d environments, got %d", jobs.MaxEnvironments, n)
	}
	seen := map[string]bool{}
	for i, e := range req.Environments {
		f := fmt.Sprintf("environments[%d]", i)
		if !jobs.ValidEnvironmentName(e.Name) {
			return f + ".name", "must match ^[a-z0-9_]+$"
		}
		if seen[e.Name] {
			return f + ".name", "environment " + e.Name + " is named twice"
		}
		seen[e.Name] = true
		if !jobs.ValidSHA(e.ExpectedSHA) {
			return f + ".expected_sha", "must be a 40-character lower-case hex commit SHA"
		}
	}
	if w := string(req.Wait); w != "true" && w != "false" {
		return "wait", "must be true (answer when finished) or false (answer 202 at once and poll)"
	}
	return "", ""
}

// create validates the request and records a queued job. With wait false it
// answers 202 with that record at once and deploys in the background; the
// caller polls GET /api/v1/deploys/{id}. With wait true it deploys, then
// answers 200 with the finished record. A deploy that ran but did not deploy
// still answers 200 with state "failed": the request worked, the deploy did
// not.
func (h *DeployHandlers) create(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxDeployBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		h.runner.ObserveRefusal("deploy", "invalid")
		if tooLarge(err) {
			failure(w, http.StatusRequestEntityTooLarge, bodyTooLarge)
			return
		}
		failure(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	// Exactly one JSON value: anything after it but whitespace is refused. The
	// read also runs into the size cap, so padding after the object is a 413.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		h.runner.ObserveRefusal("deploy", "invalid")
		if tooLarge(err) {
			failure(w, http.StatusRequestEntityTooLarge, bodyTooLarge)
			return
		}
		failure(w, http.StatusBadRequest, trailingData)
		return
	}
	if field, msg := req.validate(); field != "" {
		h.runner.ObserveRefusal("deploy", "invalid")
		unprocessable(w, field, msg)
		return
	}

	job, err := h.runner.NewJob(req.Environments)
	if err != nil {
		if errors.Is(err, jobs.ErrInvalidRequest) {
			h.runner.ObserveRefusal("deploy", "invalid")
			unprocessable(w, "environments", err.Error())
			return
		}
		failure(w, http.StatusInternalServerError, "recording deploy: "+err.Error())
		return
	}

	if !req.wait() {
		if err := h.runner.Start(job); err != nil {
			h.refuse(w, job, err)
			return
		}
		w.Header().Set("Location", "/api/v1/deploys/"+job.ID)
		writeJSON(w, http.StatusAccepted, job)
		return
	}

	job, err = h.runner.RunSync(job)
	if errors.Is(err, jobs.ErrBusy) || errors.Is(err, jobs.ErrShutdown) {
		h.refuse(w, job, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "recording deploy: " + err.Error(), "id": job.ID})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// tooLarge reports whether err is the size cap of http.MaxBytesReader.
func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// refuse answers 503 for a job the runner would not take. The runner records
// it as failed so its id never reads as queued (Runner.Refuse, PD-04).
func (h *DeployHandlers) refuse(w http.ResponseWriter, job jobs.Job, reason error) {
	if job, err := h.runner.Refuse(job, reason); err != nil {
		h.log.Error("recording a refused deploy", "job_id", job.ID, "error", err.Error())
	}
	w.Header().Set("Retry-After", "30")
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": reason.Error(), "id": job.ID})
}

// get answers one job record by id.
func (h *DeployHandlers) get(w http.ResponseWriter, r *http.Request) {
	job, err := h.runner.Store().Get(r.PathValue("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		failure(w, http.StatusNotFound, "deploy not found")
		return
	}
	if err != nil {
		failure(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job.Kind != "" { // a removal is polled under /api/v1/removals (PD-21)
		failure(w, http.StatusNotFound, "deploy not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// list answers the newest deploy records. Removals are not deploys and are
// left out (ListKind, PD-21).
func (h *DeployHandlers) list(w http.ResponseWriter, r *http.Request) {
	limit := defaultListLimit
	if q := r.URL.Query(); q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxListLimit {
			unprocessable(w, "limit", fmt.Sprintf("must be an integer from 1 to %d", maxListLimit))
			return
		}
		limit = n
	}
	list, err := h.runner.Store().ListKind("", limit)
	if err != nil {
		failure(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deploys": list})
}
