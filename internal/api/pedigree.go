package api

import (
	"errors"
	"net/http"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/service"
)

// PedigreeHandlers exposes the packaging-pedigree endpoints.
type PedigreeHandlers struct {
	svc *service.PedigreeService
}

// NewPedigreeHandlers wires the handlers.
func NewPedigreeHandlers(svc *service.PedigreeService) *PedigreeHandlers {
	return &PedigreeHandlers{svc: svc}
}

// RegisterRoutes mounts the pedigree routes.
func (h *PedigreeHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/pedigree/instances", h.registerInstance)
	mux.HandleFunc("GET /v1/pedigree/instances", h.listInstances)
	mux.HandleFunc("POST /v1/pedigree/aliases", h.registerAlias)
	mux.HandleFunc("POST /v1/pedigree/stows", h.registerStow)
	mux.HandleFunc("POST /v1/pedigree/operations", h.importOperation)
	mux.HandleFunc("GET /v1/pedigree/model", h.currentModel)
	mux.HandleFunc("GET /v1/pedigree/expand/{instance}", h.expand)
	mux.HandleFunc("GET /v1/pedigree/contents/{parent}", h.contents)
	mux.HandleFunc("POST /v1/pedigree/range-tasks", h.submitRangeTask)
	mux.HandleFunc("GET /v1/pedigree/range-tasks/{taskId}", h.getRangeTask)
	mux.HandleFunc("POST /v1/pedigree/scans/attribute", h.attributeScan)
	mux.HandleFunc("POST /v1/pedigree/revisions/impact", h.revisionImpact)
	mux.HandleFunc("GET /v1/pedigree/calcs", h.listCalcs)
	mux.HandleFunc("GET /v1/pedigree/calcs/{calcId}/replay", h.replayCalc)
}

type instanceReq struct {
	InstanceID  string     `json:"instance_id"`
	Label       string     `json:"label"`
	Issuer      string     `json:"issuer"`
	Scope       string     `json:"scope"`
	Revision    string     `json:"revision"`
	Role        string     `json:"role"`
	ActiveFrom  time.Time  `json:"active_from"`
	ActiveTo    *time.Time `json:"active_to"`
	SourceEvent string     `json:"source_event_id"`
}

func (h *PedigreeHandlers) registerInstance(w http.ResponseWriter, r *http.Request) {
	var req instanceReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	err := h.svc.RegisterInstance(r.Context(), domain.PackageInstance{
		InstanceID: req.InstanceID, Label: req.Label, Issuer: req.Issuer, Scope: req.Scope,
		Revision: req.Revision, Role: req.Role, ActiveFrom: req.ActiveFrom,
		ActiveTo: req.ActiveTo, SourceEventID: req.SourceEvent,
	})
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"instance_id": req.InstanceID, "registered": true})
}

func (h *PedigreeHandlers) listInstances(w http.ResponseWriter, r *http.Request) {
	view, err := h.svc.CurrentModel(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pedigree_model_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": view.Model.InstancesView(),
		"count":     len(view.Model.InstancesView()),
	})
}

type aliasReq struct {
	AliasID        string    `json:"alias_id"`
	FromInstanceID string    `json:"from_instance_id"`
	ToInstanceID   string    `json:"to_instance_id"`
	EffectiveAt    time.Time `json:"effective_at"`
	BasisEventID   string    `json:"basis_event_id"`
	SourceEventID  string    `json:"source_event_id"`
}

func (h *PedigreeHandlers) registerAlias(w http.ResponseWriter, r *http.Request) {
	var req aliasReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	err := h.svc.RegisterAlias(r.Context(), domain.AliasRecord{
		AliasID: req.AliasID, FromInstanceID: req.FromInstanceID, ToInstanceID: req.ToInstanceID,
		EffectiveAt: req.EffectiveAt, BasisEventID: req.BasisEventID, SourceEventID: req.SourceEventID,
	})
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"alias_id": req.AliasID, "registered": true})
}

type stowReq struct {
	StowID          string     `json:"stow_id"`
	ChildInstanceID string     `json:"child_instance_id"`
	ParentKind      string     `json:"parent_kind"`
	ParentRef       string     `json:"parent_ref"`
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to"`
	HandoverEventID string     `json:"handover_event_id"`
	SourceEventID   string     `json:"source_event_id"`
}

func (h *PedigreeHandlers) registerStow(w http.ResponseWriter, r *http.Request) {
	var req stowReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	err := h.svc.RegisterStow(r.Context(), domain.StowInterval{
		StowID: req.StowID, ChildInstanceID: req.ChildInstanceID, ParentKind: req.ParentKind,
		ParentRef: req.ParentRef, ValidFrom: req.ValidFrom, ValidTo: req.ValidTo,
		HandoverEventID: req.HandoverEventID, SourceEventID: req.SourceEventID,
	})
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"stow_id": req.StowID, "registered": true})
}

type operationReq struct {
	OperationKey    string                      `json:"operation_key"`
	Version         int                         `json:"version"`
	SupersedesKey   string                      `json:"supersedes_key"`
	Kind            string                      `json:"kind"`
	Station         string                      `json:"station"`
	OccurredAt      time.Time                   `json:"occurred_at"`
	ReceivedAt      time.Time                   `json:"received_at"`
	SourceEventID   string                      `json:"source_event_id"`
	IdempotencyKey  string                      `json:"idempotency_key"`
	Unit            string                      `json:"unit"`
	ContainerID     string                      `json:"container_id"`
	HandoverEventID string                      `json:"handover_event_id"`
	Inputs          []domain.PackageQty         `json:"inputs"`
	Outputs         []domain.PackageQty         `json:"outputs"`
	Allocations     []domain.ExplicitAllocation `json:"allocations"`
	Losses          []domain.LossRecord         `json:"losses"`
	RawPayload      map[string]any              `json:"raw_payload"`
	Note            string                      `json:"note"`
}

func (h *PedigreeHandlers) importOperation(w http.ResponseWriter, r *http.Request) {
	var req operationReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	created, err := h.svc.ImportOperation(r.Context(), domain.OperationRecord{
		OperationKey: req.OperationKey, Version: req.Version, SupersedesKey: req.SupersedesKey,
		Kind: req.Kind, Station: req.Station, OccurredAt: req.OccurredAt, ReceivedAt: req.ReceivedAt,
		SourceEventID: req.SourceEventID, IdempotencyKey: req.IdempotencyKey, Unit: req.Unit,
		ContainerID: req.ContainerID, HandoverEventID: req.HandoverEventID,
		Inputs: req.Inputs, Outputs: req.Outputs, Allocations: req.Allocations,
		Losses: req.Losses, RawPayload: req.RawPayload, Note: req.Note,
	})
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"operation_key": req.OperationKey, "version": req.Version, "created": created,
	})
}

func (h *PedigreeHandlers) currentModel(w http.ResponseWriter, r *http.Request) {
	view, err := h.svc.CurrentModel(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pedigree_model_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings":        view.Findings,
		"balances":        view.Balances,
		"weight_checks":   view.Weights,
		"finding_count":   len(view.Findings),
		"operation_count": len(view.Model.Operations),
	})
}

func (h *PedigreeHandlers) expand(w http.ResponseWriter, r *http.Request) {
	exp, err := h.svc.ExpandLineage(r.Context(), r.PathValue("instance"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lineage_expand_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (h *PedigreeHandlers) contents(w http.ResponseWriter, r *http.Request) {
	at := time.Now().UTC()
	if v := r.URL.Query().Get("at"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			at = t
		}
	}
	sum, err := h.svc.Contents(r.Context(), r.PathValue("parent"), at)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "contents_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

type rangeTaskReq struct {
	SourceInstanceID string `json:"source_instance_id"`
	TargetInstanceID string `json:"target_instance_id"`
	Unit             string `json:"unit"`
	AugmentLimit     int    `json:"augment_limit"`
}

func (h *PedigreeHandlers) submitRangeTask(w http.ResponseWriter, r *http.Request) {
	var req rangeTaskReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	task, err := h.svc.SubmitRangeTask(r.Context(), service.RangeRequest{
		SourceInstanceID: req.SourceInstanceID, TargetInstanceID: req.TargetInstanceID,
		Unit: req.Unit, AugmentLimit: req.AugmentLimit,
	})
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (h *PedigreeHandlers) getRangeTask(w http.ResponseWriter, r *http.Request) {
	task, ok := h.svc.GetTask(r.PathValue("taskId"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task_not_found", "pedigree task not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

type scanAttributionReq struct {
	EventID      string    `json:"event_id"`
	Label        string    `json:"label"`
	Station      string    `json:"station"`
	OccurredAt   time.Time `json:"occurred_at"`
	ContentScope []string  `json:"content_scope"`
}

func (h *PedigreeHandlers) attributeScan(w http.ResponseWriter, r *http.Request) {
	var req scanAttributionReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	res, err := h.svc.AttributeScan(r.Context(), domain.ScanAttributionRequest{
		EventID: req.EventID, Label: req.Label, Station: req.Station,
		OccurredAt: req.OccurredAt, ContentScope: req.ContentScope,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scan_attribution_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type revisionImpactReq struct {
	CorrectedOperationKey string `json:"corrected_operation_key"`
	NewVersion            int    `json:"new_version"`
}

func (h *PedigreeHandlers) revisionImpact(w http.ResponseWriter, r *http.Request) {
	var req revisionImpactReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	imp, err := h.svc.AnalyzeRevisionImpact(r.Context(), service.RevisionImpactRequest{
		CorrectedOperationKey: req.CorrectedOperationKey, NewVersion: req.NewVersion,
	})
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imp)
}

func (h *PedigreeHandlers) listCalcs(w http.ResponseWriter, r *http.Request) {
	calcs, err := h.svc.ListCalcs(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_calcs_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calculations": calcs, "count": len(calcs)})
}

// replayCalc recomputes a stored calculation against its pinned operation
// versions, proving a publication can reproduce its basis after revisions.
func (h *PedigreeHandlers) replayCalc(w http.ResponseWriter, r *http.Request) {
	replay, pinned, err := h.svc.ReplayCalc(r.Context(), r.PathValue("calcId"))
	if err != nil {
		h.writePedigreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pinned_calc": pinned,
		"recomputed":  replay,
		"match": replay.Lower == pinned.Lower && replay.Upper == pinned.Upper &&
			replay.Status == pinned.RangeStatus,
	})
}

func (h *PedigreeHandlers) writePedigreeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrPedigreeValidation) {
		writeErr(w, http.StatusUnprocessableEntity, "pedigree_invalid", err.Error(), nil)
		return
	}
	writeErr(w, http.StatusInternalServerError, "pedigree_error", err.Error(), nil)
}
