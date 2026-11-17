package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Calculation statuses. A range query evaluated on demand is stored as a
// calculation version that pins exactly which operation versions it used.
const (
	CalcCompleted  = "completed"
	CalcUnverified = "unverified" // 求解未完成或达到资源限制
	CalcSuperseded = "superseded" // 依赖的操作出现了修订
)

// CalcBasis pins one operation version used by a calculation.
type CalcBasis struct {
	OperationKey  string `json:"operation_key"`
	Version       int    `json:"version"`
	SourceEventID string `json:"source_event_id"`
	OccurredAt    string `json:"occurred_at"`
}

// CalculationRecord is one on-demand evaluation and its immutable basis.
// Downstream snapshots reference the calc id; a later correction never
// overwrites the row — it marks it superseded and creates a new calc.
type CalculationRecord struct {
	CalcID           string        `json:"calc_id"`
	Status           string        `json:"status"`
	SourceInstanceID string        `json:"source_instance_id"`
	TargetInstanceID string        `json:"target_instance_id"`
	Unit             string        `json:"unit"`
	Lower            int64         `json:"lower"`
	Upper            int64         `json:"upper"`
	RangeStatus      string        `json:"range_status"`
	Basis            []CalcBasis   `json:"basis_operations"`
	MissingBasis     []string      `json:"missing_basis,omitempty"`
	ConflictingOps   []string      `json:"conflicting_operations,omitempty"`
	ResourceLimit    bool          `json:"resource_limit_reached,omitempty"`
	BasisHash        string        `json:"basis_hash"`
	CreatedAt        time.Time     `json:"created_at"`
	SupersededAt     *time.Time    `json:"superseded_at,omitempty"`
	SupersededBy     string        `json:"superseded_by_calc_id,omitempty"`
	Witness          []WitnessFlow `json:"witness_allocation,omitempty"`
}

// BasisHash deterministically fingerprints the pinned operation versions so a
// recomputation on the same basis can be proven identical.
func BasisHash(basis []CalcBasis) string {
	cp := append([]CalcBasis{}, basis...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].OperationKey < cp[j].OperationKey })
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// OperationVersionView is one known version of an operation key.
type OperationVersionView struct {
	OperationKey string
	Version      int
	OccurredAt   time.Time
	ReceivedAt   time.Time
}

// RevisionImpact describes what a correction invalidates. Correction
// propagation starts at the corrected operation and follows the real
// producer->consumer chain; unrelated calculations stay valid.
type RevisionImpact struct {
	CorrectedOperationKey string   `json:"corrected_operation_key"`
	NewVersion            int      `json:"new_version"`
	AffectedOperations    []string `json:"affected_downstream_operations"`
	InvalidatedCalcIDs    []string `json:"invalidated_calc_ids"`
	UnrelatedCalcIDs      []string `json:"unaffected_calc_ids"`
	RecomputableSnapshots []string `json:"snapshots_still_recomputable_on_old_basis"`
}

// DownstreamOperations returns the corrected operation plus every operation
// that (transitively) consumes an instance it produces, in chain order.
func (m *PedigreeModel) DownstreamOperations(operationKey string) []string {
	op, ok := m.Operations[operationKey]
	if !ok {
		return nil
	}
	// Seed with the output instances of the corrected op.
	frontier := map[string]bool{}
	for _, q := range op.Outputs {
		frontier[q.InstanceID] = true
	}
	affected := map[string]bool{operationKey: true}
	queue := make([]string, 0, len(frontier))
	for id := range frontier {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		inst := queue[0]
		queue = queue[1:]
		for _, consumerKey := range m.consumers[inst] {
			if affected[consumerKey] {
				continue
			}
			affected[consumerKey] = true
			if cop, ok := m.Operations[consumerKey]; ok {
				for _, q := range cop.Outputs {
					queue = append(queue, q.InstanceID)
				}
			}
		}
	}
	out := mapKeys(affected)
	sort.Strings(out)
	return out
}

// ComputeRevisionImpact classifies stored calculations against a correction.
// A calc is invalidated only when its pinned basis contains the corrected op
// or a downstream dependent op. Existing snapshots remain recomputable because
// they reference the OLD calc id and its pinned versions.
func ComputeRevisionImpact(correctedKey string, newVersion int, affectedOps []string,
	calcs []CalculationRecord, snapshotCalcRefs map[string]string) RevisionImpact {
	affectedSet := map[string]bool{}
	for _, k := range affectedOps {
		affectedSet[k] = true
	}
	imp := RevisionImpact{
		CorrectedOperationKey: correctedKey, NewVersion: newVersion,
		AffectedOperations: append([]string{}, affectedOps...),
		InvalidatedCalcIDs: []string{}, UnrelatedCalcIDs: []string{},
		RecomputableSnapshots: []string{},
	}
	for _, c := range calcs {
		depends := false
		for _, b := range c.Basis {
			if affectedSet[b.OperationKey] {
				depends = true
				break
			}
		}
		if depends {
			imp.InvalidatedCalcIDs = append(imp.InvalidatedCalcIDs, c.CalcID)
		} else {
			imp.UnrelatedCalcIDs = append(imp.UnrelatedCalcIDs, c.CalcID)
		}
	}
	// Snapshots referencing a superseded calc keep their original basis and can
	// be replayed; list them so callers never assume a publication vanished.
	for snapID, calcID := range snapshotCalcRefs {
		for _, id := range imp.InvalidatedCalcIDs {
			if calcID == id {
				imp.RecomputableSnapshots = append(imp.RecomputableSnapshots, snapID)
			}
		}
	}
	sort.Strings(imp.InvalidatedCalcIDs)
	sort.Strings(imp.UnrelatedCalcIDs)
	sort.Strings(imp.RecomputableSnapshots)
	return imp
}

// PinnedBasis builds the calc basis for the current model.
func (m *PedigreeModel) PinnedBasis() []CalcBasis {
	out := make([]CalcBasis, 0, len(m.OpOrder))
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		out = append(out, CalcBasis{
			OperationKey: op.OperationKey, Version: op.Version,
			SourceEventID: op.SourceEventID, OccurredAt: op.OccurredAt.Format(time.RFC3339Nano),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OperationKey < out[j].OperationKey })
	return out
}

// CalcBasisSummary renders a short human summary of a pinned basis.
func CalcBasisSummary(b []CalcBasis) string {
	return fmt.Sprintf("%d operation versions pinned, basis_hash=%s", len(b), BasisHash(b)[:12])
}
