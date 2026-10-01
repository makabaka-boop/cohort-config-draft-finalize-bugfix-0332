// Package api implements the HTTP handlers for the configuration release
// service.
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"configrel/internal/configmodel"
	"configrel/internal/store"
)

// Server holds dependencies shared by handlers.
type Server struct {
	Store *store.Store
}

// NewRouter wires all endpoints.
func (s *Server) NewRouter() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)

	mux.HandleFunc("POST /v1/drafts", s.createDraft)
	mux.HandleFunc("GET /v1/drafts/{id}", s.getDraft)
	mux.HandleFunc("PUT /v1/drafts/{id}/content", s.updateDraft)
	mux.HandleFunc("POST /v1/drafts/{id}/finalize", s.finalizeDraft)

	mux.HandleFunc("GET /v1/packages/{id}", s.getPackage)

	mux.HandleFunc("POST /v1/releases", s.publish)
	mux.HandleFunc("POST /v1/releases/adjust", s.adjust)
	mux.HandleFunc("POST /v1/releases/rollback", s.rollback)
	mux.HandleFunc("GET /v1/releases", s.listReleases)
	mux.HandleFunc("GET /v1/releases/current", s.currentRelease)
	mux.HandleFunc("GET /v1/releases/{generation}", s.getRelease)
	mux.HandleFunc("GET /v1/resolve", s.resolve)

	return logRequests(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ---- drafts ----

type updateDraftRequest struct {
	Routes *[]configmodel.Route `json:"routes"`
	Limits *[]configmodel.Limit `json:"limits"`
}

func (s *Server) createDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.CreateDraft(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	d, err := s.Store.GetDraft(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) updateDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}

	current, err := s.Store.GetDraft(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if current.Status != "open" {
		writeError(w, http.StatusConflict, "draft is already finalized and immutable")
		return
	}

	var req updateDraftRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Missing fields keep the group's current value; explicit null is
	// rejected via the typed-nil pointer distinction.
	routes := current.Routes
	if req.Routes != nil {
		raw, err := json.Marshal(*req.Routes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid routes: "+err.Error())
			return
		}
		routes = raw
	}
	limits := current.Limits
	if req.Limits != nil {
		raw, err := json.Marshal(*req.Limits)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limits: "+err.Error())
			return
		}
		limits = raw
	}
	if routes == nil || limits == nil {
		writeError(w, http.StatusUnprocessableEntity,
			"both routes and limits must be provided before the draft can be used")
		return
	}

	// Validate eagerly so editors get feedback; finalize re-validates.
	var content configmodel.DraftContent
	if err := json.Unmarshal(routes, &content.Routes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid routes: "+err.Error())
		return
	}
	if err := json.Unmarshal(limits, &content.Limits); err != nil {
		writeError(w, http.StatusBadRequest, "invalid limits: "+err.Error())
		return
	}
	if problems := content.Validate(); len(problems) > 0 {
		writeValidationError(w, problems)
		return
	}

	d, err := s.Store.UpdateDraft(r.Context(), id, routes, limits)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) finalizeDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	current, err := s.Store.GetDraft(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if current.Routes == nil || current.Limits == nil {
		writeError(w, http.StatusUnprocessableEntity,
			"draft is incomplete: provide both routes and limits")
		return
	}
	var content configmodel.DraftContent
	if err := json.Unmarshal(current.Routes, &content.Routes); err != nil {
		writeError(w, http.StatusInternalServerError, "stored routes corrupt: "+err.Error())
		return
	}
	if err := json.Unmarshal(current.Limits, &content.Limits); err != nil {
		writeError(w, http.StatusInternalServerError, "stored limits corrupt: "+err.Error())
		return
	}
	if problems := content.Validate(); len(problems) > 0 {
		writeValidationError(w, problems)
		return
	}
	res, err := s.Store.FinalizeDraft(r.Context(), id, &content)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"draft":   res.Draft,
		"package": res.Package,
		"reused":  res.Reused,
	})
}

// ---- packages ----

func (s *Server) getPackage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := s.Store.GetPackage(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ---- releases ----

type publishRequest struct {
	PackageID          int64  `json:"package_id"`
	RolloutPct         *int   `json:"rollout_pct"`
	MinClientVersion   string `json:"min_client_version"`
	ExpectedGeneration int64  `json:"expected_generation"`
	Note               string `json:"note"`
}

func validateCommit(packageID int64, pct *int, minVersion string, expectPct bool) (int, string) {
	if packageID <= 0 {
		return 0, "package_id must be a positive integer"
	}
	if expectPct && pct == nil {
		return 0, "rollout_pct is required (0-100)"
	}
	p := 100
	if pct != nil {
		p = *pct
	}
	if p < 0 || p > 100 {
		return 0, "rollout_pct must be between 0 and 100"
	}
	if strings.TrimSpace(minVersion) == "" {
		return 0, "min_client_version is required"
	}
	if _, err := configmodel.CompareVersions(minVersion, minVersion); err != nil {
		return 0, "invalid min_client_version: " + err.Error()
	}
	return p, ""
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	pct, msg := validateCommit(req.PackageID, req.RolloutPct, req.MinClientVersion, true)
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	res, err := s.Store.CommitRelease(r.Context(), store.Commit{
		ExpectedGeneration: req.ExpectedGeneration,
		PackageID:          req.PackageID,
		RolloutPct:         pct,
		MinClientVersion:   req.MinClientVersion,
		Action:             store.ActionPublish,
		Note:               req.Note,
	})
	if err != nil {
		s.writeCommitError(w, r, err)
		return
	}
	s.writeCommitResult(w, r, res)
}

type adjustRequest struct {
	RolloutPct         *int    `json:"rollout_pct"`
	MinClientVersion   *string `json:"min_client_version"`
	ExpectedGeneration int64   `json:"expected_generation"`
	Note               string  `json:"note"`
}

func (s *Server) adjust(w http.ResponseWriter, r *http.Request) {
	var req adjustRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.RolloutPct == nil && req.MinClientVersion == nil {
		writeError(w, http.StatusUnprocessableEntity,
			"provide rollout_pct and/or min_client_version to adjust")
		return
	}
	head, pkg, err := s.currentSnapshot(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	pct := head.RolloutPct
	if req.RolloutPct != nil {
		pct = *req.RolloutPct
	}
	if pct < 0 || pct > 100 {
		writeError(w, http.StatusUnprocessableEntity, "rollout_pct must be between 0 and 100")
		return
	}
	minVersion := head.MinClientVersion
	if req.MinClientVersion != nil {
		minVersion = *req.MinClientVersion
	}
	if _, err := configmodel.CompareVersions(minVersion, minVersion); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid min_client_version: "+err.Error())
		return
	}
	res, err := s.Store.CommitRelease(r.Context(), store.Commit{
		ExpectedGeneration: req.ExpectedGeneration,
		PackageID:          pkg.ID,
		RolloutPct:         pct,
		MinClientVersion:   minVersion,
		Action:             store.ActionAdjust,
		Note:               req.Note,
	})
	if err != nil {
		s.writeCommitError(w, r, err)
		return
	}
	s.writeCommitResult(w, r, res)
}

type rollbackRequest struct {
	PackageID          int64   `json:"package_id"`
	RolloutPct         *int    `json:"rollout_pct"`
	MinClientVersion   *string `json:"min_client_version"`
	ExpectedGeneration int64   `json:"expected_generation"`
	Note               string  `json:"note"`
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.PackageID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "package_id must reference a complete snapshot")
		return
	}
	// The rollback target must be a complete package previously published.
	target, err := s.Store.GetPackage(r.Context(), req.PackageID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	prior, err := s.Store.LastReleaseForPackage(r.Context(), req.PackageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity,
				"cannot roll back to a package that was never published")
			return
		}
		writeStoreError(w, err)
		return
	}
	pct := 100
	if req.RolloutPct != nil {
		pct = *req.RolloutPct
	}
	if pct < 0 || pct > 100 {
		writeError(w, http.StatusUnprocessableEntity, "rollout_pct must be between 0 and 100")
		return
	}
	// Default minimum version is the one the target package carried when
	// last published.
	minVersion := prior.MinClientVersion
	if req.MinClientVersion != nil {
		minVersion = *req.MinClientVersion
	}
	if _, err := configmodel.CompareVersions(minVersion, minVersion); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid min_client_version: "+err.Error())
		return
	}
	res, err := s.Store.CommitRelease(r.Context(), store.Commit{
		ExpectedGeneration: req.ExpectedGeneration,
		PackageID:          target.ID,
		RolloutPct:         pct,
		MinClientVersion:   minVersion,
		Action:             store.ActionRollback,
		Note:               req.Note,
	})
	if err != nil {
		s.writeCommitError(w, r, err)
		return
	}
	s.writeCommitResult(w, r, res)
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	releases, err := s.Store.ListReleases(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	head, _ := s.Store.HeadGeneration(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"head_generation": head,
		"releases":        releases,
	})
}

func (s *Server) currentRelease(w http.ResponseWriter, r *http.Request) {
	head, err := s.Store.HeadGeneration(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if head == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"head_generation": 0, "release": nil})
		return
	}
	rel, err := s.Store.GetReleaseWithPackage(r.Context(), head)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"head_generation": head,
		"release":         rel,
	})
}

func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("generation")
	gen, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || gen <= 0 {
		writeError(w, http.StatusBadRequest, "invalid generation: "+raw)
		return
	}
	rel, err := s.Store.GetReleaseWithPackage(r.Context(), gen)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// resolve serves a client: GET /v1/resolve?client_id=...&client_version=...
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := strings.TrimSpace(q.Get("client_id"))
	clientVersion := strings.TrimSpace(q.Get("client_version"))
	if clientID == "" {
		writeError(w, http.StatusBadRequest, "client_id query parameter is required")
		return
	}
	if clientVersion == "" {
		writeError(w, http.StatusBadRequest, "client_version query parameter is required")
		return
	}
	if _, err := configmodel.CompareVersions(clientVersion, clientVersion); err != nil {
		writeError(w, http.StatusBadRequest, "invalid client_version: "+err.Error())
		return
	}

	snaps, err := s.Store.ListReleaseSnapshots(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	head, err := s.Store.HeadGeneration(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if head == 0 || len(snaps) == 0 {
		writeError(w, http.StatusNotFound, "no configuration has been published yet")
		return
	}
	chosen, err := configmodel.Resolve(snaps, clientVersion, clientID, head)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if chosen == nil {
		writeError(w, http.StatusNotFound,
			"no compatible configuration package for this client version")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"package_id":         chosen.Package.ID,
		"generation":         chosen.Release.Generation,
		"head_generation":    head,
		"served_head":        chosen.Head,
		"rollout_pct":        chosen.Release.RolloutPct,
		"min_client_version": chosen.Release.MinClientVersion,
		"reason":             chosen.Reason,
		"checksum":           chosen.Package.Checksum,
		"routes":             chosen.Package.Routes,
		"limits":             chosen.Package.Limits,
	})
}

// ---- helpers ----

func (s *Server) currentSnapshot(r *http.Request) (*configmodel.Release, *configmodel.Package, error) {
	head, err := s.Store.HeadGeneration(r.Context())
	if err != nil {
		return nil, nil, err
	}
	if head == 0 {
		return nil, nil, store.ErrNotFound
	}
	rel, err := s.Store.GetReleaseWithPackage(r.Context(), head)
	if err != nil {
		return nil, nil, err
	}
	return rel, rel.Package, nil
}

func (s *Server) writeCommitResult(w http.ResponseWriter, r *http.Request, res *store.CommitResult) {
	// Embed the complete package so every response points at a full snapshot.
	full, err := s.Store.GetReleaseWithPackage(r.Context(), res.Release.Generation)
	rel := res.Release
	if err == nil {
		rel = full
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"generation":          res.Release.Generation,
		"previous_generation": prevGen(res.Prev),
		"release":             rel,
	})
}

func (s *Server) writeCommitError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrConflict) {
		head, _ := s.Store.HeadGeneration(r.Context())
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":              err.Error(),
			"current_generation": head,
		})
		return
	}
	writeStoreError(w, err)
}

func prevGen(r *configmodel.Release) int64 {
	if r == nil {
		return 0
	}
	return r.Generation
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id: "+raw)
		return 0, false
	}
	return id, true
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing JSON content")
	}
	return nil
}

func writeValidationError(w http.ResponseWriter, problems []string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":    "validation failed",
		"problems": problems,
	})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("store error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
