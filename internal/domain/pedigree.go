// Package domain — packaging pedigree (包装谱系) primitives.
//
// This file is deliberately separate from the security-event model: pedigree
// answers "which original shipments does a package that exists RIGHT NOW come
// from, and how certain can we be about each source quantity". It never
// performs physical split/merge work and never rules on cargo release.
//
// The four operation semantics are kept strictly apart because the existing
// connected-path query conflates them:
//
//   - relabel (改标签): same goods, new label. Creates an evidence-backed alias,
//     adds no goods and no quantity flow.
//   - intact_stow (完整装载): a whole package is placed into a container/outer
//     package. Inner packages are NOT consumed and sibling packages never
//     exchange evidence.
//   - split (拆分): one or more input packages are physically broken into
//     output packages; input->output quantity flow is real.
//   - mixed_repack (混合重组): inner items from several sources are combined
//     and re-issued; only this kind (plus split) creates quantity flow, and
//     when no item-level allocation is supplied only feasible ranges exist.
package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Pedigree operation kinds.
const (
	OpRelabel     = "relabel"      // 改标签
	OpIntactStow  = "intact_stow"  // 完整装载（不混合内容）
	OpSplit       = "split"        // 拆分
	OpMixedRepack = "mixed_repack" // 混合重组
)

// IsKnownOperationKind reports whether k is one of the four pedigree semantics.
func IsKnownOperationKind(k string) bool {
	switch k {
	case OpRelabel, OpIntactStow, OpSplit, OpMixedRepack:
		return true
	}
	return false
}

// Standard quantity families. Quantities may only be reconciled inside one
// convertible family. Package counts and inner-item counts live in different
// families and must never be netted against each other; weight is its own
// family and is never an item-count allocation.
const (
	FamilyItem    = "item"    // 可换算内件
	FamilyPackage = "package" // 包装件数
	FamilyWeight  = "weight"  // 重量
)

// UnitDef declares one unit and its conversion factor to the base unit of its
// family (factor * unit = base). Integer factors keep every reconciled
// quantity integral — half an indivisible item is not an admissible answer.
type UnitDef struct {
	Unit   string `json:"unit"`
	Family string `json:"family"`
	Factor int64  `json:"factor_to_base"`
}

// DefaultUnits is the registry the platform ships with. Callers may extend it
// through the service, but a unit absent from the registry stays
// unconvertible: its quantities are held as "pending verification" rather
// than forced into a balance.
func DefaultUnits() []UnitDef {
	return []UnitDef{
		{Unit: "PCS", Family: FamilyItem, Factor: 1},    // 内件（件）
		{Unit: "DOZEN", Family: FamilyItem, Factor: 12}, // 打（可换算内件）
		{Unit: "PACKAGE", Family: FamilyPackage, Factor: 1},
		{Unit: "G", Family: FamilyWeight, Factor: 1}, // 克为重量族基准单位
		{Unit: "KG", Family: FamilyWeight, Factor: 1000},
	}
}

// UnitRegistry converts quantities inside a family.
type UnitRegistry struct {
	defs map[string]UnitDef
}

// NewUnitRegistry builds a registry from defs. A duplicate unit definition is
// rejected so the conversion table cannot silently disagree with itself.
func NewUnitRegistry(defs []UnitDef) (*UnitRegistry, error) {
	r := &UnitRegistry{defs: map[string]UnitDef{}}
	for _, d := range defs {
		if d.Unit == "" || d.Family == "" || d.Factor <= 0 {
			return nil, fmt.Errorf("invalid unit definition %+v", d)
		}
		if _, exists := r.defs[d.Unit]; exists {
			return nil, fmt.Errorf("duplicate unit %q", d.Unit)
		}
		r.defs[d.Unit] = d
	}
	return r, nil
}

// FamilyOf returns the family of a unit and whether the unit is registered.
func (r *UnitRegistry) FamilyOf(unit string) (string, bool) {
	d, ok := r.defs[strings.TrimSpace(unit)]
	if !ok {
		return "", false
	}
	return d.Family, true
}

// ToBase converts qty in unit to the family base unit. An unknown unit yields
// ok=false so callers can mark the balance pending instead of guessing.
func (r *UnitRegistry) ToBase(unit string, qty int64) (int64, string, bool) {
	d, ok := r.defs[strings.TrimSpace(unit)]
	if !ok {
		return 0, "", false
	}
	return qty * d.Factor, d.Family, true
}

// Convertible reports whether two units share a family (their quantities may
// appear in the same conservation equation).
func (r *UnitRegistry) Convertible(a, b string) bool {
	fa, oka := r.FamilyOf(a)
	fb, okb := r.FamilyOf(b)
	return oka && okb && fa == fb
}

// WeightMeasure is one weighing. Basis says what was on the scale (gross/net/
// tare); tolerance is the declared measurement tolerance in the same unit.
type WeightMeasure struct {
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	Basis     string  `json:"basis"` // gross|net|tare
	Tolerance float64 `json:"tolerance,omitempty"`
}

// PackageQty is one package participation in an operation.
type PackageQty struct {
	InstanceID  string         `json:"instance_id"`
	Label       string         `json:"label"`
	Station     string         `json:"station,omitempty"`
	Quantity    int64          `json:"quantity"` // always integral; negative is invalid
	Unit        string         `json:"unit"`
	GrossWeight *WeightMeasure `json:"gross_weight,omitempty"`
	TareWeight  *WeightMeasure `json:"tare_weight,omitempty"`
}

// ExplicitAllocation is a source->target quantity the source document actually
// states. It is evidence: ranges may only be tightened by rows of this kind,
// never by proportional guessing.
type ExplicitAllocation struct {
	FromInstanceID  string `json:"from_instance_id"`
	ToInstanceID    string `json:"to_instance_id"`
	Quantity        int64  `json:"quantity"`
	Unit            string `json:"unit"`
	EvidenceEventID string `json:"evidence_event_id"`
}

// LossRecord is a documented, evidenced loss (有凭证损耗). Undocumented
// shrinkage is not accepted: it shows up as a conservation failure.
type LossRecord struct {
	InstanceID      string `json:"instance_id"`
	Quantity        int64  `json:"quantity"`
	Unit            string `json:"unit"`
	EvidenceEventID string `json:"evidence_event_id"`
	Reason          string `json:"reason,omitempty"`
}

// OperationRecord is one version of one packaging operation. Corrections
// append a NEW row with the same OperationKey and a higher Version; the old
// row is never modified, which is what lets an old publication be recomputed
// on exactly the basis it originally used.
type OperationRecord struct {
	OperationKey    string               `json:"operation_key"`
	Version         int                  `json:"version"`
	SupersedesKey   string               `json:"supersedes_operation_key,omitempty"`
	Kind            string               `json:"kind"`
	Station         string               `json:"station"`
	OccurredAt      time.Time            `json:"occurred_at"`
	ReceivedAt      time.Time            `json:"received_at"`
	SourceEventID   string               `json:"source_event_id"`
	IdempotencyKey  string               `json:"idempotency_key"`
	Inputs          []PackageQty         `json:"inputs"`
	Outputs         []PackageQty         `json:"outputs"`
	Unit            string               `json:"unit"` // reconciliation unit (inner-item family)
	ContainerID     string               `json:"container_id,omitempty"`
	Allocations     []ExplicitAllocation `json:"allocations,omitempty"`
	Losses          []LossRecord         `json:"losses,omitempty"`
	HandoverEventID string               `json:"handover_event_id,omitempty"`
	RawPayload      map[string]any       `json:"raw_payload,omitempty"`
	Note            string               `json:"note,omitempty"`
}

// Validate performs structural validation. Semantic conservation is checked
// later by the reconciler, which needs unit conversion and chain context.
func (o *OperationRecord) Validate() error {
	if strings.TrimSpace(o.OperationKey) == "" {
		return fmt.Errorf("operation_key is required")
	}
	if !IsKnownOperationKind(o.Kind) {
		return fmt.Errorf("unknown operation kind %q", o.Kind)
	}
	if o.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	nonNeg := func(qs []PackageQty, name string) error {
		for _, q := range qs {
			if q.Quantity < 0 {
				return fmt.Errorf("%s quantity must be a non-negative integer", name)
			}
			if q.Unit == "" {
				return fmt.Errorf("%s %s missing unit", name, q.InstanceID)
			}
		}
		return nil
	}
	if err := nonNeg(o.Inputs, "input"); err != nil {
		return err
	}
	if err := nonNeg(o.Outputs, "output"); err != nil {
		return err
	}
	for _, a := range o.Allocations {
		if a.Quantity < 0 {
			return fmt.Errorf("allocation quantity must be a non-negative integer")
		}
		if a.EvidenceEventID == "" {
			return fmt.Errorf("explicit allocation %s->%s carries no evidence event",
				a.FromInstanceID, a.ToInstanceID)
		}
	}
	switch o.Kind {
	case OpRelabel:
		if len(o.Inputs) != 1 || len(o.Outputs) != 1 {
			return fmt.Errorf("relabel must have exactly one input and one output package")
		}
	case OpIntactStow:
		if len(o.Inputs) == 0 {
			return fmt.Errorf("intact_stow needs at least one whole input package")
		}
		if o.ContainerID == "" && len(o.Outputs) == 0 {
			return fmt.Errorf("intact_stow needs a container_id or an outer-package output")
		}
	case OpSplit, OpMixedRepack:
		if len(o.Inputs) == 0 || len(o.Outputs) == 0 {
			return fmt.Errorf("%s needs inputs and outputs", o.Kind)
		}
		if o.Unit == "" {
			return fmt.Errorf("%s needs a reconciliation unit", o.Kind)
		}
	}
	return nil
}

// PackageInstance is a physical package identity. A label alone is NOT an
// identity: the same number re-used at another station is a different
// instance. Instances are co-located by issuer (签发方), the number's
// applicability scope (编号适用范围, normally a station) and the interval the
// instance is active (启用区间).
type PackageInstance struct {
	InstanceID    string     `json:"instance_id"`
	Label         string     `json:"label"`
	Issuer        string     `json:"issuer"`
	Scope         string     `json:"scope"` // station/scope in which the number is applicable
	Revision      string     `json:"revision,omitempty"`
	Role          string     `json:"role"` // package|outer_package|container
	ActiveFrom    time.Time  `json:"active_from"`
	ActiveTo      *time.Time `json:"active_to,omitempty"`
	SourceEventID string     `json:"source_event_id"`
}

// ActiveAt reports whether the instance is the valid identity at t.
func (p PackageInstance) ActiveAt(t time.Time) bool {
	if t.Before(p.ActiveFrom) {
		return false
	}
	if p.ActiveTo != nil && !t.Before(*p.ActiveTo) {
		return false
	}
	return true
}

// AliasRecord is an evidence-backed label change (换标签别名). Without a basis
// event no alias is accepted — aliases are claims that must be auditable.
type AliasRecord struct {
	AliasID        string    `json:"alias_id"`
	FromInstanceID string    `json:"from_instance_id"`
	ToInstanceID   string    `json:"to_instance_id"`
	EffectiveAt    time.Time `json:"effective_at"`
	BasisEventID   string    `json:"basis_event_id"`
	SourceEventID  string    `json:"source_event_id"`
}

// StowInterval is one identity relation: child placed inside parent (a
// container or an outer package) for [ValidFrom, ValidTo). Relations are
// interval-dated so nested queries traverse only levels that are actually
// effective at the queried instant.
type StowInterval struct {
	StowID          string     `json:"stow_id"`
	ChildInstanceID string     `json:"child_instance_id"`
	ParentKind      string     `json:"parent_kind"` // container|outer_package
	ParentRef       string     `json:"parent_ref"`  // container key or outer-package instance id
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	HandoverEventID string     `json:"handover_event_id,omitempty"`
	SourceEventID   string     `json:"source_event_id"`
}

// ActiveAt reports whether the stow relation holds at t.
func (s StowInterval) ActiveAt(t time.Time) bool {
	if t.Before(s.ValidFrom) {
		return false
	}
	if s.ValidTo != nil && !t.Before(*s.ValidTo) {
		return false
	}
	return true
}

// Overlaps reports whether two half-open intervals overlap. A zero end means
// open-ended.
func intervalsOverlap(from1 time.Time, to1 *time.Time, from2 time.Time, to2 *time.Time) bool {
	if to1 != nil && !from2.Before(*to1) {
		return false
	}
	if to2 != nil && !from1.Before(*to2) {
		return false
	}
	return true
}

// InstanceResolver finds the instance meant by a label observed at a station
// and time. It deliberately requires scope + interval agreement so a number
// re-used at another station can never be merged into one instance.
type InstanceResolver struct {
	byID      map[string]PackageInstance
	byLabel   map[string][]PackageInstance // key: scope + "\x00" + label
	aliases   []AliasRecord
	aliasFrom map[string][]AliasRecord // from instance id -> aliases (newest first)
}

// NewInstanceResolver indexes instances and aliases.
func NewInstanceResolver(instances []PackageInstance, aliases []AliasRecord) *InstanceResolver {
	r := &InstanceResolver{
		byID:      map[string]PackageInstance{},
		byLabel:   map[string][]PackageInstance{},
		aliasFrom: map[string][]AliasRecord{},
		aliases:   append([]AliasRecord{}, aliases...),
	}
	for _, in := range instances {
		r.byID[in.InstanceID] = in
		k := in.Scope + "\x00" + in.Label
		r.byLabel[k] = append(r.byLabel[k], in)
	}
	for _, a := range aliases {
		r.aliasFrom[a.FromInstanceID] = append(r.aliasFrom[a.FromInstanceID], a)
	}
	for k := range r.aliasFrom {
		sort.SliceStable(r.aliasFrom[k], func(i, j int) bool {
			return r.aliasFrom[k][i].EffectiveAt.After(r.aliasFrom[k][j].EffectiveAt)
		})
	}
	return r
}

// ResolveResult explains how a scan label was resolved.
type ResolveResult struct {
	InstanceID string `json:"instance_id"`
	Match      string `json:"match"` // active|via_alias|retired|unknown
	Reason     string `json:"reason,omitempty"`
	// AliasTrail lists the alias chain followed when an old label was presented.
	AliasTrail []string `json:"alias_trail,omitempty"`
}

// Resolve maps (label, station, observed time) to the instance valid then.
// When the label names an instance already retired by that time it returns
// match=retired (never a silent jump to a successor); callers must not invent
// child-level attributions from that result.
func (r *InstanceResolver) Resolve(label, station string, at time.Time) ResolveResult {
	k := station + "\x00" + label
	cands := r.byLabel[k]
	// Fall back to any scope when the scan omits the station.
	if len(cands) == 0 {
		for _, ins := range r.byID {
			if ins.Label == label {
				cands = append(cands, ins)
			}
		}
	}
	if len(cands) == 0 {
		return ResolveResult{Match: "unknown", Reason: "no instance carries this label in the cited scope"}
	}
	var active, retired []PackageInstance
	for _, in := range cands {
		if in.ActiveAt(at) {
			active = append(active, in)
		} else {
			retired = append(retired, in)
		}
	}
	switch {
	case len(active) == 1:
		return ResolveResult{InstanceID: active[0].InstanceID, Match: "active"}
	case len(active) > 1:
		// Two distinct instances simultaneously valid under one label/scope is
		// itself an identity defect; surface it instead of picking one.
		return ResolveResult{Match: "unknown", Reason: "multiple simultaneously active instances share label+scope"}
	case len(retired) > 0:
		// Follow aliases when the relabel was effective by the observation time
		// and has a documented basis.
		for _, old := range retired {
			for _, a := range r.aliasFrom[old.InstanceID] {
				if !at.Before(a.EffectiveAt) {
					if cur, ok := r.byID[a.ToInstanceID]; ok && cur.ActiveAt(at) {
						return ResolveResult{
							InstanceID: cur.InstanceID, Match: "via_alias",
							AliasTrail: []string{old.InstanceID, cur.InstanceID},
							Reason:     "label superseded by a documented relabel alias",
						}
					}
				}
			}
		}
		return ResolveResult{InstanceID: retired[0].InstanceID, Match: "retired",
			Reason: "label names an instance whose active interval ended; no alias maps it to a current instance"}
	}
	return ResolveResult{Match: "unknown", Reason: "label matches no active or retired instance at this time"}
}

// Instance returns the indexed instance by id.
func (r *InstanceResolver) Instance(id string) (PackageInstance, bool) {
	in, ok := r.byID[id]
	return in, ok
}
