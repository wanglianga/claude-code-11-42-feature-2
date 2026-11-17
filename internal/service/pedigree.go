package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/store"

	"github.com/google/uuid"
)

// PedigreeService orchestrates the packaging-pedigree use cases. It evaluates
// on demand against an immutable model, persists every calculation version and
// keeps corrections from overwriting the basis a publication referenced.
type PedigreeService struct {
	store *store.PedigreeStore
	now   func() time.Time
}

// NewPedigreeService builds the service.
func NewPedigreeService(ps *store.PedigreeStore) *PedigreeService {
	return &PedigreeService{store: ps, now: time.Now().UTC}
}

// ErrPedigreeValidation is returned when an import fails structural checks.
var ErrPedigreeValidation = errors.New("pedigree validation error")

// RegisterInstance stores a package instance.
func (s *PedigreeService) RegisterInstance(ctx context.Context, in domain.PackageInstance) error {
	if in.InstanceID == "" || in.Label == "" || in.Issuer == "" || in.Scope == "" {
		return fmt.Errorf("%w: instance id, label, issuer and scope are required", ErrPedigreeValidation)
	}
	if in.Role == "" {
		in.Role = "package"
	}
	return s.store.InsertInstance(ctx, in)
}

// RegisterAlias stores a documented relabel alias. An alias without a basis
// event is rejected so undocumented identity merges cannot enter the graph.
func (s *PedigreeService) RegisterAlias(ctx context.Context, a domain.AliasRecord) error {
	if a.AliasID == "" || a.FromInstanceID == "" || a.ToInstanceID == "" || a.BasisEventID == "" {
		return fmt.Errorf("%w: alias id, both instances and a basis event are required", ErrPedigreeValidation)
	}
	return s.store.InsertAlias(ctx, a)
}

// RegisterStow stores an interval-dated containment relation.
func (s *PedigreeService) RegisterStow(ctx context.Context, st domain.StowInterval) error {
	if st.StowID == "" || st.ChildInstanceID == "" || st.ParentRef == "" {
		return fmt.Errorf("%w: stow id, child instance and parent reference are required", ErrPedigreeValidation)
	}
	return s.store.InsertStow(ctx, st)
}

// ImportOperation validates and appends an operation version. A correction
// (same operation key) gets the next version number and records the prior key.
func (s *PedigreeService) ImportOperation(ctx context.Context, op domain.OperationRecord) (bool, error) {
	if op.OperationKey == "" || op.Kind == "" || op.SourceEventID == "" || op.IdempotencyKey == "" {
		return false, fmt.Errorf("%w: operation key, kind, source event and idempotency key are required", ErrPedigreeValidation)
	}
	if !knownOpKind(op.Kind) {
		return false, fmt.Errorf("%w: unknown operation kind %q", ErrPedigreeValidation, op.Kind)
	}
	if err := op.Validate(); err != nil {
		return false, fmt.Errorf("%w: %v", ErrPedigreeValidation, err)
	}
	if op.ReceivedAt.IsZero() {
		op.ReceivedAt = s.now()
	}
	if op.Version <= 0 {
		next, err := s.store.LatestOperationVersion(ctx, op.OperationKey)
		if err != nil {
			return false, err
		}
		op.Version = next + 1
		if next > 0 {
			op.SupersedesKey = op.OperationKey
		}
	}
	return s.store.InsertOperation(ctx, op)
}

func knownOpKind(k string) bool {
	switch k {
	case domain.OpRelabel, domain.OpIntactStow, domain.OpSplit, domain.OpMixedRepack:
		return true
	}
	return false
}

// loadModel assembles the immutable pedigree model from every stored row.
func (s *PedigreeService) loadModel(ctx context.Context, cutoff time.Time, pinned map[string]int) (*domain.PedigreeModel, error) {
	reg, err := domain.NewUnitRegistry(domain.DefaultUnits())
	if err != nil {
		return nil, err
	}
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	aliases, err := s.store.ListAliases(ctx)
	if err != nil {
		return nil, err
	}
	stows, err := s.store.ListStows(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := s.store.ListOperationVersions(ctx)
	if err != nil {
		return nil, err
	}
	return domain.BuildModel(domain.ModelInputs{
		Units: reg, Instances: instances, Aliases: aliases, Stows: stows,
		Operations: ops, CutoffKnowledge: cutoff, PinnedVersions: pinned,
	})
}

// ModelView is the assembled model plus its check results.
type ModelView struct {
	Model    *domain.PedigreeModel
	Findings []domain.PedigreeFinding
	Balances []domain.QuantityBalance
	Weights  []domain.WeightConclusion
}

// CurrentModel returns the newest consistent model and all its check results.
func (s *PedigreeService) CurrentModel(ctx context.Context) (*ModelView, error) {
	m, err := s.loadModel(ctx, time.Time{}, nil)
	if err != nil {
		return nil, err
	}
	return &ModelView{
		Model: m, Findings: m.Findings(), Balances: m.Balances(), Weights: m.WeightConclusions(),
	}, nil
}

// ExpandLineage returns the lineage sub-graph reaching a surviving package.
func (s *PedigreeService) ExpandLineage(ctx context.Context, targetInstance string) (domain.Expansion, error) {
	m, err := s.loadModel(ctx, time.Time{}, nil)
	if err != nil {
		return domain.Expansion{}, err
	}
	return m.Expand(targetInstance), nil
}

// Contents returns the non-double-counted contents of a container at a time.
func (s *PedigreeService) Contents(ctx context.Context, parentRef string, at time.Time) (domain.ContentsSummary, error) {
	m, err := s.loadModel(ctx, time.Time{}, nil)
	if err != nil {
		return domain.ContentsSummary{}, err
	}
	if at.IsZero() {
		at = s.now()
	}
	return m.ContentsAt(parentRef, at), nil
}

// RangeRequest is the on-demand source-range query input.
type RangeRequest struct {
	SourceInstanceID string
	TargetInstanceID string
	Unit             string
	AugmentLimit     int
}

// RangeTask is the handle of an on-demand evaluation.
type RangeTask struct {
	TaskID    string
	Status    string
	Progress  int
	Total     int
	CalcID    string
	Result    *domain.SourceRange
	StartedAt time.Time
}

// task registry is in-memory: evaluations run synchronously but expose a task
// handle and progress so a deep chain can report "in progress" / "unverified"
// rather than blocking silently.
type pedigreeTask struct {
	handle RangeTask
	model  *domain.PedigreeModel
	req    RangeRequest
}

// SubmitRangeTask records a task and runs the evaluation, persisting a calc
// version. It returns a handle with the completed/unverified result.
func (s *PedigreeService) SubmitRangeTask(ctx context.Context, req RangeRequest) (*RangeTask, error) {
	if req.SourceInstanceID == "" || req.TargetInstanceID == "" {
		return nil, fmt.Errorf("%w: source and target instance are required", ErrPedigreeValidation)
	}
	m, err := s.loadModel(ctx, time.Time{}, nil)
	if err != nil {
		return nil, err
	}
	taskID := uuid.NewString()
	handle := RangeTask{
		TaskID: taskID, Status: "running", Progress: 0, Total: 2,
		StartedAt: s.now(),
	}
	tasksMu.Lock()
	pedigreeTasks[taskID] = &pedigreeTask{handle: handle, model: m, req: req}
	tasksMu.Unlock()

	// Step 1: model assembled; step 2: solve the range.
	s.updateProgress(taskID, 1, 2)
	rng := m.SourceRange(domain.SourceRangeQuery{
		SourceInstanceID: req.SourceInstanceID,
		TargetInstanceID: req.TargetInstanceID,
		Unit:             req.Unit,
		AugmentLimit:     req.AugmentLimit,
	})
	calcStatus := domain.CalcCompleted
	if rng.Status == "unverified" || rng.ResourceLimit {
		calcStatus = domain.CalcUnverified
	}
	calc := domain.CalculationRecord{
		CalcID: uuid.NewString(), Status: calcStatus,
		SourceInstanceID: rng.SourceInstanceID, TargetInstanceID: rng.TargetInstanceID,
		Unit: rng.Unit, Lower: rng.Lower, Upper: rng.Upper, RangeStatus: rng.Status,
		Basis: m.PinnedBasis(), BasisHash: domain.BasisHash(m.PinnedBasis()),
		MissingBasis: rng.MissingBasis, ConflictingOps: rng.ConflictingOps,
		Witness: rng.Witness, ResourceLimit: rng.ResourceLimit, CreatedAt: s.now(),
	}
	if err := s.store.InsertCalc(ctx, calc); err != nil {
		return nil, err
	}
	rngCopy := rng
	handle.CalcID = calc.CalcID
	handle.Result = &rngCopy
	handle.Status = calcStatus
	handle.Progress = 2
	tasksMu.Lock()
	pedigreeTasks[taskID].handle = handle
	tasksMu.Unlock()
	return &handle, nil
}

// GetTask returns a task handle with its progress.
func (s *PedigreeService) GetTask(taskID string) (RangeTask, bool) {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	t, ok := pedigreeTasks[taskID]
	if !ok {
		return RangeTask{}, false
	}
	return t.handle, true
}

func (s *PedigreeService) updateProgress(taskID string, progress, total int) {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	if t, ok := pedigreeTasks[taskID]; ok {
		t.handle.Progress = progress
		t.handle.Total = total
	}
}

// AttributeScan binds a scan to the instance effective at scan time.
func (s *PedigreeService) AttributeScan(ctx context.Context, req domain.ScanAttributionRequest) (domain.ScanAttributionResult, error) {
	m, err := s.loadModel(ctx, time.Time{}, nil)
	if err != nil {
		return domain.ScanAttributionResult{}, err
	}
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return domain.ScanAttributionResult{}, err
	}
	aliases, err := s.store.ListAliases(ctx)
	if err != nil {
		return domain.ScanAttributionResult{}, err
	}
	resolver := domain.NewInstanceResolver(instances, aliases)
	return m.AttributeScan(*resolver, req), nil
}

// RevisionImpactRequest triggers the dependency-scoped invalidation analysis.
type RevisionImpactRequest struct {
	CorrectedOperationKey string
	NewVersion            int
}

// AnalyzeRevisionImpact returns which downstream operations and stored
// calculations a correction invalidates, while confirming that the snapshots
// referencing old calcs remain recomputable on their pinned basis.
func (s *PedigreeService) AnalyzeRevisionImpact(ctx context.Context, req RevisionImpactRequest) (domain.RevisionImpact, error) {
	if req.CorrectedOperationKey == "" {
		return domain.RevisionImpact{}, fmt.Errorf("%w: corrected operation key is required", ErrPedigreeValidation)
	}
	m, err := s.loadModel(ctx, time.Time{}, nil)
	if err != nil {
		return domain.RevisionImpact{}, err
	}
	affected := m.DownstreamOperations(req.CorrectedOperationKey)
	calcs, err := s.store.ListCalcs(ctx, "")
	if err != nil {
		return domain.RevisionImpact{}, err
	}
	refs, err := s.store.SnapshotCalcRefs(ctx)
	if err != nil {
		return domain.RevisionImpact{}, err
	}
	imp := domain.ComputeRevisionImpact(req.CorrectedOperationKey, req.NewVersion, affected, calcs, refs)
	// Persist the supersession markers for invalidated calcs.
	for _, calcID := range imp.InvalidatedCalcIDs {
		if err := s.store.MarkCalcSuperseded(ctx, calcID, uuid.NewString(), s.now()); err != nil {
			return imp, err
		}
	}
	return imp, nil
}

// ListCalcs returns stored calculation versions.
func (s *PedigreeService) ListCalcs(ctx context.Context, status string) ([]domain.CalculationRecord, error) {
	return s.store.ListCalcs(ctx, status)
}

// ReplayCalc recomputes a source range against the EXACT operation versions a
// stored calculation pinned, ignoring later revisions. It proves that a
// publication referencing the old calc can still reproduce its basis after a
// correction has superseded it.
func (s *PedigreeService) ReplayCalc(ctx context.Context, calcID string) (*domain.SourceRange, *domain.CalculationRecord, error) {
	calcs, err := s.store.ListCalcs(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	var pinned *domain.CalculationRecord
	for i := range calcs {
		if calcs[i].CalcID == calcID {
			pinned = &calcs[i]
			break
		}
	}
	if pinned == nil {
		return nil, nil, fmt.Errorf("%w: calc %s not found", ErrPedigreeValidation, calcID)
	}
	pinnedVersions := map[string]int{}
	for _, b := range pinned.Basis {
		pinnedVersions[b.OperationKey] = b.Version
	}
	m, err := s.loadModel(ctx, time.Time{}, pinnedVersions)
	if err != nil {
		return nil, nil, err
	}
	rng := m.SourceRange(domain.SourceRangeQuery{
		SourceInstanceID: pinned.SourceInstanceID,
		TargetInstanceID: pinned.TargetInstanceID,
		Unit:             pinned.Unit,
	})
	if rng.Lower != pinned.Lower || rng.Upper != pinned.Upper || rng.Status != pinned.RangeStatus {
		return nil, pinned, fmt.Errorf("replay mismatch: pinned [%d,%d] %s vs recomputed [%d,%d] %s",
			pinned.Lower, pinned.Upper, pinned.RangeStatus, rng.Lower, rng.Upper, rng.Status)
	}
	return &rng, pinned, nil
}
