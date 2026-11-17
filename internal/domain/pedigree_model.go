package domain

import (
	"fmt"
	"sort"
	"time"
)

// Pedigree finding codes. These describe data defects; the platform never
// deletes an edge to manufacture a valid path — a bad operation stays visible.
const (
	FindConservationMismatch = "conservation_mismatch"          // 投入≠产出+留存+有凭证损耗
	FindUnitUnconvertible    = "quantity_unit_unconvertible"    // 无法换算，保持待核
	FindPackageItemNetting   = "package_item_netting"           // 包装件数与内件数量直接相抵
	FindAllocExceeds         = "allocation_exceeds_capacity"    // 明确分配超过包装容量
	FindTimeInversion        = "operation_time_inversion"       // 时间倒置
	FindSelfReflow           = "self_reflow"                    // 自我回流
	FindOpCycle              = "operation_cycle"                // 操作链成环
	FindMutexPosition        = "mutex_position_overlap"         // 同一完整包装同时占两个互斥位置
	FindTareUnverifiable     = "tare_unverifiable"              // 皮重缺失，毛重变化无法判定
	FindWeightTolerance      = "weight_outside_tolerance"       // 超出测量容差的净重变化
	FindDuplicateCount       = "duplicate_participation"        // 同一包装在一个操作内重复计数
	FindUnknownInstance      = "unknown_package_instance"       // 引用了不存在的实例
	FindReusedLabel          = "reused_label_distinct_instance" // 异地同号，未并为一件
)

// Finding severities.
const (
	PedSevInfo    = "info"
	PedSevWarning = "warning"
	PedSevError   = "error"
	PedSevBlocker = "blocker"
)

// PedigreeFinding is one核对结论 with the operations that support it.
type PedigreeFinding struct {
	Code         string         `json:"code"`
	Severity     string         `json:"severity"`
	OperationKey string         `json:"operation_key,omitempty"`
	RelatedOps   []string       `json:"related_operations,omitempty"`
	InstanceID   string         `json:"instance_id,omitempty"`
	Message      string         `json:"message"`
	Detail       map[string]any `json:"detail,omitempty"`
}

// QuantityBalance is the reconciled conservation result for one operation.
type QuantityBalance struct {
	OperationKey   string   `json:"operation_key"`
	Family         string   `json:"family"`
	BaseUnit       string   `json:"base_unit"`
	InputTotal     int64    `json:"input_total_base"`
	OutputTotal    int64    `json:"output_total_base"`
	LossTotal      int64    `json:"documented_loss_total_base"`
	Delta          int64    `json:"delta"` // input - output - loss; zero means conserved
	Pending        bool     `json:"pending_verification"`
	PendingReasons []string `json:"pending_reasons,omitempty"`
}

// WeightConclusion explains a gross-weight change without auto-judging loss.
type WeightConclusion struct {
	InstanceID   string  `json:"instance_id"`
	OperationKey string  `json:"operation_key"`
	NetBefore    float64 `json:"net_before,omitempty"`
	NetAfter     float64 `json:"net_after,omitempty"`
	Delta        float64 `json:"net_delta,omitempty"`
	Tolerance    float64 `json:"tolerance,omitempty"`
	Conclusion   string  `json:"conclusion"` // within_tolerance|outside_tolerance|tare_unverifiable|rebox_info
	Message      string  `json:"message"`
}

// PedigreeModel is the assembled, version-pinned view of every packaging
// operation and identity relation. All range queries run against an immutable
// model so a concurrent correction cannot change the answer mid-computation.
type PedigreeModel struct {
	Units      *UnitRegistry
	Instances  map[string]PackageInstance
	Aliases    []AliasRecord
	Stows      []StowInterval
	Operations map[string]OperationRecord // key -> selected version
	OpOrder    []string                   // operation keys in occurred order
	findings   []PedigreeFinding
	balances   []QuantityBalance
	weights    []WeightConclusion
	// producers maps an instance id to the operation key that first produced it.
	producers map[string]string
	// consumers maps an instance id to operation keys that consume it, in order.
	consumers map[string][]string
}

// ModelInputs is everything the builder needs. Operations may contain several
// versions of one key; the builder pins the newest version known at the cutoff.
type ModelInputs struct {
	Units      *UnitRegistry
	Instances  []PackageInstance
	Aliases    []AliasRecord
	Stows      []StowInterval
	Operations []OperationRecord
	// CutoffKnowledge selects versions by received_at. Zero means "newest".
	CutoffKnowledge time.Time
	// PinnedVersions forces specific operation versions (recomputation of an
	// old calculation). Empty means take the newest visible version.
	PinnedVersions map[string]int
}

// BuildModel assembles a consistent, immutable pedigree model and runs every
// structural and conservation check. Range solving is a separate step so the
// (potentially expensive) solver only runs when a query actually asks for it.
func BuildModel(in ModelInputs) (*PedigreeModel, error) {
	if in.Units == nil {
		reg, err := NewUnitRegistry(DefaultUnits())
		if err != nil {
			return nil, err
		}
		in.Units = reg
	}
	m := &PedigreeModel{
		Units:      in.Units,
		Instances:  map[string]PackageInstance{},
		Operations: map[string]OperationRecord{},
		producers:  map[string]string{},
		consumers:  map[string][]string{},
	}
	for _, ins := range in.Instances {
		m.Instances[ins.InstanceID] = ins
	}
	m.Aliases = append([]AliasRecord{}, in.Aliases...)
	m.Stows = append([]StowInterval{}, in.Stows...)

	// Pin one version per operation key. When PinnedVersions is set, operation
	// keys absent from the pin are excluded entirely: a replay of an old calc
	// must run against exactly the operations that existed in its basis, so an
	// unrelated later contradiction cannot make the replay infeasible.
	latest := map[string]OperationRecord{}
	for _, op := range in.Operations {
		if !in.CutoffKnowledge.IsZero() && op.ReceivedAt.After(in.CutoffKnowledge) {
			continue
		}
		if len(in.PinnedVersions) > 0 {
			pin, ok := in.PinnedVersions[op.OperationKey]
			if !ok || op.Version != pin {
				continue
			}
		}
		cur, exists := latest[op.OperationKey]
		if !exists || op.Version > cur.Version {
			latest[op.OperationKey] = op
		}
	}
	for k, op := range latest {
		if err := op.Validate(); err != nil {
			m.findings = append(m.findings, PedigreeFinding{
				Code: FindUnknownInstance, Severity: PedSevError, OperationKey: k,
				Message: "operation failed structural validation: " + err.Error(),
			})
			continue
		}
		m.Operations[k] = op
	}
	m.OpOrder = make([]string, 0, len(m.Operations))
	for k := range m.Operations {
		m.OpOrder = append(m.OpOrder, k)
	}
	sort.Slice(m.OpOrder, func(i, j int) bool {
		a, b := m.Operations[m.OpOrder[i]], m.Operations[m.OpOrder[j]]
		if a.OccurredAt.Equal(b.OccurredAt) {
			return a.OperationKey < b.OperationKey
		}
		return a.OccurredAt.Before(b.OccurredAt)
	})

	m.indexLifecycle()
	m.checkReferences()
	m.reconcileQuantities()
	m.checkWeights()
	m.checkTemporal()
	m.checkPositions()
	return m, nil
}

// Findings returns every核对发现 (deterministic order).
func (m *PedigreeModel) Findings() []PedigreeFinding {
	out := append([]PedigreeFinding{}, m.findings...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].OperationKey < out[j].OperationKey
	})
	return out
}

// Balances returns the per-operation quantity conservation results.
func (m *PedigreeModel) Balances() []QuantityBalance {
	return append([]QuantityBalance{}, m.balances...)
}

// WeightConclusions returns the gross/tare weight reasoning rows.
func (m *PedigreeModel) WeightConclusions() []WeightConclusion {
	return append([]WeightConclusion{}, m.weights...)
}

func (m *PedigreeModel) addFinding(f PedigreeFinding) { m.findings = append(m.findings, f) }

// indexLifecycle records who produces and who consumes each instance so the
// chain graph, time-inversion and revision-impact queries share one basis.
func (m *PedigreeModel) indexLifecycle() {
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		seen := map[string]bool{}
		for _, q := range op.Outputs {
			if seen[q.InstanceID] {
				m.addFinding(PedigreeFinding{
					Code: FindDuplicateCount, Severity: PedSevWarning, OperationKey: k,
					InstanceID: q.InstanceID,
					Message:    "output package listed more than once in one operation; its quantity must not be double counted",
				})
			}
			seen[q.InstanceID] = true
			if _, ok := m.producers[q.InstanceID]; !ok {
				m.producers[q.InstanceID] = k
			}
		}
		seenIn := map[string]bool{}
		for _, q := range op.Inputs {
			if seenIn[q.InstanceID] {
				m.addFinding(PedigreeFinding{
					Code: FindDuplicateCount, Severity: PedSevWarning, OperationKey: k,
					InstanceID: q.InstanceID,
					Message:    "input package listed more than once in one operation; its quantity must not be double counted",
				})
			}
			seenIn[q.InstanceID] = true
			m.consumers[q.InstanceID] = append(m.consumers[q.InstanceID], k)
		}
	}
}

func (m *PedigreeModel) checkReferences() {
	known := func(id string) bool {
		_, ok := m.Instances[id]
		return ok
	}
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		for _, q := range append(append([]PackageQty{}, op.Inputs...), op.Outputs...) {
			if !known(q.InstanceID) {
				m.addFinding(PedigreeFinding{
					Code: FindUnknownInstance, Severity: PedSevError, OperationKey: k,
					InstanceID: q.InstanceID,
					Message:    "operation references a package instance that was never registered",
				})
			}
		}
		for _, a := range op.Allocations {
			if !known(a.FromInstanceID) || !known(a.ToInstanceID) {
				m.addFinding(PedigreeFinding{
					Code: FindUnknownInstance, Severity: PedSevError, OperationKey: k,
					Message: fmt.Sprintf("explicit allocation %s->%s references an unknown instance",
						a.FromInstanceID, a.ToInstanceID),
				})
			}
		}
	}
	// Reused labels across scopes are distinct instances by construction; make
	// the deliberate non-merge visible as info evidence.
	byLabel := map[string][]string{}
	for id, ins := range m.Instances {
		byLabel[ins.Label] = append(byLabel[ins.Label], id)
	}
	for label, ids := range byLabel {
		scopes := map[string]bool{}
		for _, id := range ids {
			scopes[m.Instances[id].Scope] = true
		}
		if len(scopes) > 1 {
			m.addFinding(PedigreeFinding{
				Code: FindReusedLabel, Severity: PedSevInfo, InstanceID: label,
				Message: "same label is active under distinct scopes/stations; kept as separate instances and never merged",
				Detail:  map[string]any{"instances": ids, "scopes": mapKeys(scopes)},
			})
		}
	}
}

// reconcileQuantities enforces conservation in one convertible inner-item
// family. Package counts and item counts are never netted; unknown units stay
// pending rather than being forced to balance.
func (m *PedigreeModel) reconcileQuantities() {
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		switch op.Kind {
		case OpRelabel:
			m.relabelBalance(op)
		case OpIntactStow:
			// Whole-package stowage consumes nothing and creates no inner-item
			// flow. Only verify every participant uses a coherent package unit.
			for _, q := range op.Inputs {
				if fam, ok := m.Units.FamilyOf(q.Unit); ok && fam != FamilyPackage {
					m.addFinding(PedigreeFinding{
						Code: FindPackageItemNetting, Severity: PedSevWarning, OperationKey: k,
						InstanceID: q.InstanceID,
						Message:    "intact stowage must be expressed in whole-package units, not inner-item quantities",
					})
				}
			}
		case OpSplit, OpMixedRepack:
			m.flowBalance(op)
		}
	}
}

func (m *PedigreeModel) relabelBalance(op OperationRecord) {
	in, out := op.Inputs[0], op.Outputs[0]
	b := QuantityBalance{OperationKey: op.OperationKey, Family: FamilyItem}
	famIn, okIn := m.Units.FamilyOf(in.Unit)
	famOut, okOut := m.Units.FamilyOf(out.Unit)
	if !okIn || !okOut || famIn != famOut {
		b.Pending = true
		b.PendingReasons = []string{"relabel quantities use unconvertible units; content equality held pending verification"}
		m.balances = append(m.balances, b)
		m.addFinding(PedigreeFinding{
			Code: FindUnitUnconvertible, Severity: PedSevWarning, OperationKey: op.OperationKey,
			Message: "relabel cannot prove goods unchanged because the two units do not convert",
		})
		return
	}
	ib, _, _ := m.Units.ToBase(in.Unit, in.Quantity)
	ob, base, _ := m.Units.ToBase(out.Unit, out.Quantity)
	b.Family, b.BaseUnit, b.InputTotal, b.OutputTotal = famIn, base, ib, ob
	b.Delta = ib - ob
	m.balances = append(m.balances, b)
	if b.Delta != 0 {
		// A relabel that changes quantity is contradictory: it adds no goods.
		m.addFinding(PedigreeFinding{
			Code: FindConservationMismatch, Severity: PedSevError, OperationKey: op.OperationKey,
			Message: fmt.Sprintf("relabel must preserve quantity exactly (input %d vs output %d base units)", ib, ob),
			Detail:  map[string]any{"input_base": ib, "output_base": ob},
		})
	}
}

func (m *PedigreeModel) flowBalance(op OperationRecord) {
	b := QuantityBalance{OperationKey: op.OperationKey}
	opFamily, ok := m.Units.FamilyOf(op.Unit)
	if !ok {
		b.Pending = true
		b.PendingReasons = []string{"operation reconciliation unit is not registered"}
		m.balances = append(m.balances, b)
		m.addFinding(PedigreeFinding{
			Code: FindUnitUnconvertible, Severity: PedSevError, OperationKey: op.OperationKey,
			Message: "operation unit " + op.Unit + " is unknown; conservation cannot be evaluated",
		})
		return
	}
	b.Family = opFamily
	convert := func(q PackageQty) (int64, bool, string) {
		fam, known := m.Units.FamilyOf(q.Unit)
		if !known {
			return 0, false, "unregistered unit " + q.Unit
		}
		if fam != opFamily {
			if fam == FamilyPackage || opFamily == FamilyPackage {
				return 0, false, "package counts cannot be netted against inner-item quantities"
			}
			return 0, false, fmt.Sprintf("unit %s belongs to family %s, balance uses %s", q.Unit, fam, opFamily)
		}
		v, base, _ := m.Units.ToBase(q.Unit, q.Quantity)
		b.BaseUnit = base
		return v, true, ""
	}
	pending := []string{}
	addSide := func(qs []PackageQty, isInput bool) {
		for _, q := range qs {
			v, ok, reason := convert(q)
			if !ok {
				pending = append(pending, fmt.Sprintf("%s: %s", q.InstanceID, reason))
				sev := PedSevWarning
				code := FindUnitUnconvertible
				if reason == "package counts cannot be netted against inner-item quantities" {
					sev = PedSevError
					code = FindPackageItemNetting
				}
				m.addFinding(PedigreeFinding{
					Code: code, Severity: sev, OperationKey: op.OperationKey,
					InstanceID: q.InstanceID, Message: reason,
				})
				continue
			}
			if isInput {
				b.InputTotal += v
			} else {
				b.OutputTotal += v
			}
		}
	}
	addSide(op.Inputs, true)
	addSide(op.Outputs, false)
	lossByInstance := map[string]int64{}
	for _, l := range op.Losses {
		fam, known := m.Units.FamilyOf(l.Unit)
		if !known || fam != opFamily {
			pending = append(pending, fmt.Sprintf("loss %s: unit not convertible to balance family", l.InstanceID))
			continue
		}
		v, base, _ := m.Units.ToBase(l.Unit, l.Quantity)
		b.BaseUnit = base
		b.LossTotal += v
		lossByInstance[l.InstanceID] += v
	}
	b.Delta = b.InputTotal - b.OutputTotal - b.LossTotal
	b.Pending = len(pending) > 0
	b.PendingReasons = pending
	m.balances = append(m.balances, b)
	if !b.Pending && b.Delta != 0 {
		m.addFinding(PedigreeFinding{
			Code: FindConservationMismatch, Severity: PedSevError, OperationKey: op.OperationKey,
			Message: fmt.Sprintf("inputs %d != outputs %d + documented loss %d (delta %d %s)",
				b.InputTotal, b.OutputTotal, b.LossTotal, b.Delta, b.BaseUnit),
			Detail: map[string]any{
				"input_total": b.InputTotal, "output_total": b.OutputTotal,
				"loss_total": b.LossTotal, "delta": b.Delta, "base_unit": b.BaseUnit,
			},
		})
	}
	// Explicit allocations must fit inside their input/output package totals.
	inCap := map[string]int64{}
	outCap := map[string]int64{}
	for _, q := range op.Inputs {
		if v, ok, _ := convert(q); ok {
			inCap[q.InstanceID] += v
		}
	}
	for _, q := range op.Outputs {
		if v, ok, _ := convert(q); ok {
			outCap[q.InstanceID] += v
		}
	}
	allocIn := map[string]int64{}
	allocOut := map[string]int64{}
	for _, a := range op.Allocations {
		fam, known := m.Units.FamilyOf(a.Unit)
		if !known || fam != opFamily {
			m.addFinding(PedigreeFinding{
				Code: FindUnitUnconvertible, Severity: PedSevError, OperationKey: op.OperationKey,
				Message: fmt.Sprintf("allocation %s->%s uses a unit outside the balance family",
					a.FromInstanceID, a.ToInstanceID),
			})
			continue
		}
		v, _, _ := m.Units.ToBase(a.Unit, a.Quantity)
		allocIn[a.FromInstanceID] += v
		allocOut[a.ToInstanceID] += v
	}
	for id, v := range allocIn {
		if v > inCap[id] {
			m.addFinding(PedigreeFinding{
				Code: FindAllocExceeds, Severity: PedSevError, OperationKey: op.OperationKey,
				InstanceID: id,
				Message: fmt.Sprintf("explicit allocations from %s total %d but the package only contributes %d",
					id, v, inCap[id]),
			})
		}
	}
	for id, v := range allocOut {
		if v > outCap[id] {
			m.addFinding(PedigreeFinding{
				Code: FindAllocExceeds, Severity: PedSevError, OperationKey: op.OperationKey,
				InstanceID: id,
				Message: fmt.Sprintf("explicit allocations into %s total %d but the package only holds %d",
					id, v, outCap[id]),
			})
		}
	}
}

// checkWeights reasons about gross-weight changes. A re-boxed package is
// allowed a different gross weight: tare and weighing basis decide whether
// anything can be inferred about the content. Unknown tare => pending, never
// "loss".
func (m *PedigreeModel) checkWeights() {
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		if op.Kind != OpSplit && op.Kind != OpMixedRepack && op.Kind != OpRelabel {
			continue
		}
		inNet := map[string]float64{}
		inTareKnown := map[string]bool{}
		for _, q := range op.Inputs {
			if q.GrossWeight == nil {
				continue
			}
			tare := 0.0
			if q.TareWeight != nil && q.TareWeight.Unit == q.GrossWeight.Unit {
				tare = q.TareWeight.Value
				inTareKnown[q.InstanceID] = true
			}
			inNet[q.InstanceID] = q.GrossWeight.Value - tare
		}
		for _, q := range op.Outputs {
			if q.GrossWeight == nil {
				continue
			}
			outTare := 0.0
			tareKnown := q.TareWeight != nil && q.TareWeight.Unit == q.GrossWeight.Unit
			if tareKnown {
				outTare = q.TareWeight.Value
			}
			outNet := q.GrossWeight.Value - outTare
			// Relabel keeps the same goods: compare the single pair directly.
			if op.Kind == OpRelabel {
				in := op.Inputs[0]
				if in.GrossWeight == nil {
					continue
				}
				inTare := 0.0
				inKnown := in.TareWeight != nil && in.TareWeight.Unit == in.GrossWeight.Unit
				if inKnown {
					inTare = in.TareWeight.Value
				}
				m.weightRow(op.OperationKey, q.InstanceID, in.GrossWeight.Value-inTare, outNet,
					q.GrossWeight.Tolerance, inKnown && tareKnown)
				continue
			}
			// For split/repack an output net weight cannot map 1:1 to one input;
			// only a gross/tare basis defect is reported, never an automatic loss.
			if !tareKnown {
				m.weights = append(m.weights, WeightConclusion{
					InstanceID: q.InstanceID, OperationKey: op.OperationKey,
					Conclusion: "tare_unverifiable",
					Message:    "new box gross weight cannot be compared to content without a tare weight",
				})
				m.addFinding(PedigreeFinding{
					Code: FindTareUnverifiable, Severity: PedSevWarning, OperationKey: op.OperationKey,
					InstanceID: q.InstanceID,
					Message:    "re-box gross weight differs but tare/weighing basis is missing; no content-loss conclusion is drawn",
				})
			}
			_ = inNet
		}
	}
}

func (m *PedigreeModel) weightRow(opKey, instance string, before, after, tol float64, tareKnown bool) {
	wc := WeightConclusion{
		InstanceID: instance, OperationKey: opKey,
		NetBefore: before, NetAfter: after, Delta: after - before, Tolerance: tol,
	}
	if !tareKnown {
		wc.Conclusion = "tare_unverifiable"
		wc.Message = "tare weight missing on one side; gross-weight change is held pending, not judged as loss"
		m.addFinding(PedigreeFinding{
			Code: FindTareUnverifiable, Severity: PedSevWarning, OperationKey: opKey,
			InstanceID: instance, Message: wc.Message,
		})
	} else if absFloat(wc.Delta) <= tol {
		wc.Conclusion = "within_tolerance"
		wc.Message = "net content weight unchanged within the declared measurement tolerance"
	} else {
		wc.Conclusion = "outside_tolerance"
		wc.Message = "net content weight changed beyond tolerance; recorded for reconciliation, no release decision is made"
		m.addFinding(PedigreeFinding{
			Code: FindWeightTolerance, Severity: PedSevWarning, OperationKey: opKey,
			InstanceID: instance, Message: wc.Message,
			Detail: map[string]any{"delta": wc.Delta, "tolerance": tol},
		})
	}
	m.weights = append(m.weights, wc)
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// checkTemporal finds time inversions and self-reflows along the real
// producer->consumer chain. Edges are kept (the bad operation is reported, not
// deleted) so the defect stays auditable.
func (m *PedigreeModel) checkTemporal() {
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		if op.Kind == OpIntactStow {
			continue // stowage has no producer/consumer quantity chain
		}
		for _, q := range op.Inputs {
			// Self-reflow: a flow operation lists its own output as an input.
			for _, o := range op.Outputs {
				if o.InstanceID == q.InstanceID && op.Kind != OpRelabel {
					m.addFinding(PedigreeFinding{
						Code: FindSelfReflow, Severity: PedSevError, OperationKey: k,
						InstanceID: q.InstanceID,
						Message:    "package flows back into itself within one operation without a relabel basis",
					})
				}
			}
			producer, ok := m.producers[q.InstanceID]
			if !ok || producer == k {
				continue
			}
			p := m.Operations[producer]
			if op.OccurredAt.Before(p.OccurredAt) {
				m.addFinding(PedigreeFinding{
					Code: FindTimeInversion, Severity: PedSevError, OperationKey: k,
					RelatedOps: []string{producer}, InstanceID: q.InstanceID,
					Message: fmt.Sprintf("operation at %s consumes %s produced later at %s by %s",
						op.OccurredAt.Format(time.RFC3339), q.InstanceID,
						p.OccurredAt.Format(time.RFC3339), producer),
					Detail: map[string]any{
						"consumer_occurred_at": op.OccurredAt, "producer_occurred_at": p.OccurredAt,
					},
				})
			}
		}
	}
	// Cycle detection over the operation graph (a cycle makes chain ordering
	// impossible even if timestamps happen to look consistent).
	color := map[string]int{} // 0 white, 1 gray, 2 black
	var stack []string
	var cycle []string
	var dfs func(k string) bool
	adj := map[string][]string{}
	for id, pk := range m.producers {
		for _, ck := range m.consumers[id] {
			adj[pk] = append(adj[pk], ck)
		}
	}
	for k := range adj {
		sort.Strings(adj[k])
	}
	dfs = func(k string) bool {
		color[k] = 1
		stack = append(stack, k)
		for _, nx := range adj[k] {
			if color[nx] == 1 {
				start := 0
				for i, x := range stack {
					if x == nx {
						start = i
						break
					}
				}
				cycle = append(append([]string{}, stack[start:]...), nx)
				return true
			}
			if color[nx] == 0 && dfs(nx) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[k] = 2
		return false
	}
	for _, k := range m.OpOrder {
		if color[k] == 0 && dfs(k) {
			break
		}
	}
	if len(cycle) > 0 {
		m.addFinding(PedigreeFinding{
			Code: FindOpCycle, Severity: PedSevBlocker,
			RelatedOps: cycle,
			Message:    "operation dependency graph contains a cycle; chain-wide allocation has no consistent ordering",
			Detail:     map[string]any{"cycle": cycle},
		})
	}
}

// checkPositions enforces the exclusive-location rule: one intact package
// cannot occupy two mutually exclusive positions over the same interval.
// Sequential transloads are legal when a handover basis closes the old
// interval before the new one opens.
func (m *PedigreeModel) checkPositions() {
	byChild := map[string][]StowInterval{}
	for _, s := range m.Stows {
		byChild[s.ChildInstanceID] = append(byChild[s.ChildInstanceID], s)
	}
	for child, list := range byChild {
		sort.SliceStable(list, func(i, j int) bool { return list[i].ValidFrom.Before(list[j].ValidFrom) })
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				a, b := list[i], list[j]
				if a.ParentRef == b.ParentRef {
					continue // same position restatement, not a conflict
				}
				if intervalsOverlap(a.ValidFrom, a.ValidTo, b.ValidFrom, b.ValidTo) {
					m.addFinding(PedigreeFinding{
						Code: FindMutexPosition, Severity: PedSevError, InstanceID: child,
						RelatedOps: []string{a.SourceEventID, b.SourceEventID},
						Message: fmt.Sprintf("package occupies %s and %s over overlapping intervals with no closing handover",
							a.ParentRef, b.ParentRef),
						Detail: map[string]any{
							"position_a": a.ParentRef, "position_b": b.ParentRef,
							"from_a": a.ValidFrom, "from_b": b.ValidFrom,
						},
					})
				}
			}
		}
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// InstancesView returns the registered package instances sorted by id.
func (m *PedigreeModel) InstancesView() []PackageInstance {
	out := make([]PackageInstance, 0, len(m.Instances))
	for _, in := range m.Instances {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// Operations returns the pinned operation records in chain order.
func (m *PedigreeModel) OperationsView() []OperationRecord {
	out := make([]OperationRecord, 0, len(m.OpOrder))
	for _, k := range m.OpOrder {
		out = append(out, m.Operations[k])
	}
	return out
}
