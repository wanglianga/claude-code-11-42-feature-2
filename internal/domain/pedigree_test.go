package domain

import (
	"testing"
	"time"
)

func pedigreeBaseTime() time.Time { return time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC) }

func testRegistry(t *testing.T) *UnitRegistry {
	t.Helper()
	reg, err := NewUnitRegistry(DefaultUnits())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

// buildMixedChain creates A(10)+B(6) -> X(8)+Y(8) as a mixed_repack with no
// item-level allocation, the core "ranges only" scenario.
func buildMixedChain(t *testing.T, withAlloc bool, conserve bool) (*PedigreeModel, OperationRecord) {
	t.Helper()
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	instances := []PackageInstance{
		{InstanceID: "A", Label: "LBL-A", Issuer: "iss", Scope: "PEK", Role: "package", ActiveFrom: now.Add(-time.Hour), SourceEventID: "ev-a"},
		{InstanceID: "B", Label: "LBL-B", Issuer: "iss", Scope: "PEK", Role: "package", ActiveFrom: now.Add(-time.Hour), SourceEventID: "ev-b"},
		{InstanceID: "X", Label: "LBL-X", Issuer: "iss", Scope: "PEK", Role: "package", ActiveFrom: now, SourceEventID: "ev-x"},
		{InstanceID: "Y", Label: "LBL-Y", Issuer: "iss", Scope: "PEK", Role: "package", ActiveFrom: now, SourceEventID: "ev-y"},
	}
	outY := int64(8)
	if !conserve {
		outY = 9 // inputs 16 vs outputs 17 -> contradiction
	}
	op := OperationRecord{
		OperationKey: "OP-MIX-1", Version: 1, Kind: OpMixedRepack, Station: "PEK",
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-mix", IdempotencyKey: "mix-1",
		Unit: "PCS",
		Inputs: []PackageQty{
			{InstanceID: "A", Label: "LBL-A", Quantity: 10, Unit: "PCS"},
			{InstanceID: "B", Label: "LBL-B", Quantity: 6, Unit: "PCS"},
		},
		Outputs: []PackageQty{
			{InstanceID: "X", Label: "LBL-X", Quantity: 8, Unit: "PCS"},
			{InstanceID: "Y", Label: "LBL-Y", Quantity: outY, Unit: "PCS"},
		},
	}
	if withAlloc {
		op.Allocations = []ExplicitAllocation{
			{FromInstanceID: "A", ToInstanceID: "X", Quantity: 5, Unit: "PCS", EvidenceEventID: "ev-alloc-1"},
			{FromInstanceID: "B", ToInstanceID: "X", Quantity: 3, Unit: "PCS", EvidenceEventID: "ev-alloc-1"},
			{FromInstanceID: "A", ToInstanceID: "Y", Quantity: 5, Unit: "PCS", EvidenceEventID: "ev-alloc-1"},
			{FromInstanceID: "B", ToInstanceID: "Y", Quantity: 3, Unit: "PCS", EvidenceEventID: "ev-alloc-1"},
		}
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: instances, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	return m, op
}

func TestMixedRepackUnknownAllocationRanges(t *testing.T) {
	m, _ := buildMixedChain(t, false, true)

	ax := m.SourceRange(SourceRangeQuery{SourceInstanceID: "A", TargetInstanceID: "X"})
	// A contributes 10 into X/Y which each hold 8. A must keep at least
	// 10-8=2 in X because Y cannot absorb all 10; at most all 8 of X can be A.
	if ax.Status != "partial" || ax.Lower != 2 || ax.Upper != 8 {
		t.Fatalf("A->X want partial [2,8], got %s [%d,%d]", ax.Status, ax.Lower, ax.Upper)
	}
	// A non-exact range must still name the missing allocation basis rather
	// than imply certainty.
	if len(ax.MissingBasis) == 0 {
		t.Fatalf("open range must name the missing allocation basis")
	}
	bx := m.SourceRange(SourceRangeQuery{SourceInstanceID: "B", TargetInstanceID: "X"})
	// B only contributes 6, so its share of X cannot exceed 6.
	if bx.Lower != 0 || bx.Upper != 6 {
		t.Fatalf("B->X want [0,6], got [%d,%d]", bx.Lower, bx.Upper)
	}
	if len(bx.MissingBasis) == 0 {
		t.Fatalf("uncertain B->X range must name the missing allocation basis")
	}
	// A lower of 0 with a positive upper means "not determined", never
	// "confirmed contains" or "confirmed does not contain".
	if bx.Status != "uncertain" {
		t.Fatalf("B->X must be uncertain (0..6), got %s", bx.Status)
	}
	if ax.Status == "exact" || ax.Status == "absent" {
		t.Fatalf("open range must not be reported as exact/absent")
	}
	// The per-package uppers may not be summed into a source total: A's total
	// is 10 although A->X and A->Y each allow 8.
	ay := m.SourceRange(SourceRangeQuery{SourceInstanceID: "A", TargetInstanceID: "Y"})
	if ax.Upper+ay.Upper <= 10 {
		t.Fatalf("summed uppers must exceed the source total (got %d+%d=%d)", ax.Upper, ay.Upper, ax.Upper+ay.Upper)
	}
	// Every reported boundary has a full-chain feasible witness.
	if len(ax.Witness) == 0 {
		t.Fatalf("upper bound must carry a complete feasible allocation witness")
	}
}

func TestExplicitAllocationTightensToExact(t *testing.T) {
	m, _ := buildMixedChain(t, true, true)
	ax := m.SourceRange(SourceRangeQuery{SourceInstanceID: "A", TargetInstanceID: "X"})
	if ax.Status != "exact" || ax.Lower != 5 || ax.Upper != 5 {
		t.Fatalf("A->X want exact 5, got %s [%d,%d]", ax.Status, ax.Lower, ax.Upper)
	}
	by := m.SourceRange(SourceRangeQuery{SourceInstanceID: "B", TargetInstanceID: "Y"})
	if by.Status != "exact" || by.Lower != 3 {
		t.Fatalf("B->Y want exact 3, got %s [%d,%d]", by.Status, by.Lower, by.Upper)
	}
}

func TestContradictoryVoucherMakesChainInfeasible(t *testing.T) {
	m, _ := buildMixedChain(t, false, false)
	var mismatch bool
	for _, f := range m.Findings() {
		if f.Code == FindConservationMismatch {
			mismatch = true
		}
	}
	if !mismatch {
		t.Fatalf("inputs 16 vs outputs 17 must produce a conservation finding")
	}
	ax := m.SourceRange(SourceRangeQuery{SourceInstanceID: "A", TargetInstanceID: "X"})
	if ax.Status != "infeasible" {
		t.Fatalf("want infeasible, got %s", ax.Status)
	}
	if len(ax.ConflictingOps) == 0 {
		t.Fatalf("infeasible answer must name the conflicting operation set")
	}
}

func TestRelabelAddsNoGoodsAndAliasesIdentity(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	instances := []PackageInstance{
		{InstanceID: "P1", Label: "OLD", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), ActiveTo: ptrTime(now), SourceEventID: "ev-1"},
		{InstanceID: "P2", Label: "NEW", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "ev-2"},
	}
	aliases := []AliasRecord{{AliasID: "AL1", FromInstanceID: "P1", ToInstanceID: "P2", EffectiveAt: now, BasisEventID: "ev-relabel", SourceEventID: "ev-relabel"}}
	op := OperationRecord{
		OperationKey: "OP-REL-1", Version: 1, Kind: OpRelabel, Station: "PEK",
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-relabel", IdempotencyKey: "rel-1",
		Inputs:  []PackageQty{{InstanceID: "P1", Label: "OLD", Quantity: 4, Unit: "PCS"}},
		Outputs: []PackageQty{{InstanceID: "P2", Label: "NEW", Quantity: 4, Unit: "PCS"}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: instances, Aliases: aliases, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, f := range m.Findings() {
		t.Fatalf("clean relabel must have no findings, got %+v", f)
	}
	r := NewInstanceResolver(instances, aliases)
	res := r.Resolve("OLD", "PEK", now.Add(time.Minute))
	if res.Match != "via_alias" || res.InstanceID != "P2" {
		t.Fatalf("old label after relabel must resolve via alias to P2, got %+v", res)
	}
}

func TestIntactStowNoFlowAndNoSiblingPropagation(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	instances := []PackageInstance{
		{InstanceID: "S1", Label: "S1", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e1"},
		{InstanceID: "S2", Label: "S2", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e2"},
	}
	stows := []StowInterval{
		{StowID: "ST1", ChildInstanceID: "S1", ParentKind: "container", ParentRef: "ULD-1", ValidFrom: now, SourceEventID: "e-stow1"},
		{StowID: "ST2", ChildInstanceID: "S2", ParentKind: "container", ParentRef: "ULD-1", ValidFrom: now, SourceEventID: "e-stow2"},
	}
	op := OperationRecord{
		OperationKey: "OP-STOW-1", Version: 1, Kind: OpIntactStow, Station: "PEK",
		OccurredAt: now, ReceivedAt: now, SourceEventID: "e-stow1", IdempotencyKey: "stow-1",
		ContainerID: "ULD-1",
		Inputs: []PackageQty{
			{InstanceID: "S1", Label: "S1", Quantity: 1, Unit: "PACKAGE"},
			{InstanceID: "S2", Label: "S2", Quantity: 1, Unit: "PACKAGE"},
		},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: instances, Stows: stows, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(m.flowOps()) != 0 {
		t.Fatalf("intact stowage must create no quantity-flow operation")
	}
	for _, f := range m.Findings() {
		t.Fatalf("intact stowage must be clean, got %+v", f)
	}
	exp := m.Expand("S1")
	for _, e := range exp.Edges {
		if e.Kind == "flow" {
			t.Fatalf("stowage expansion must not contain a flow edge: %+v", e)
		}
	}
}

func TestNestedContainmentCountsLeavesOnce(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	instances := []PackageInstance{
		{InstanceID: "OUT", Label: "OUT", Issuer: "iss", Scope: "PEK", Role: "outer_package", ActiveFrom: now, SourceEventID: "e"},
		{InstanceID: "IN1", Label: "IN1", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
		{InstanceID: "IN2", Label: "IN2", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
	}
	stows := []StowInterval{
		{StowID: "s1", ChildInstanceID: "OUT", ParentKind: "container", ParentRef: "ULD-9", ValidFrom: now, SourceEventID: "e"},
		{StowID: "s2", ChildInstanceID: "IN1", ParentKind: "outer_package", ParentRef: "OUT", ValidFrom: now, SourceEventID: "e"},
		{StowID: "s3", ChildInstanceID: "IN2", ParentKind: "outer_package", ParentRef: "OUT", ValidFrom: now, SourceEventID: "e"},
	}
	ops := []OperationRecord{
		{OperationKey: "OP-P1", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-time.Hour),
			ReceivedAt: now.Add(-time.Hour), SourceEventID: "p1", IdempotencyKey: "p1", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "ROOT", Label: "ROOT", Quantity: 7, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "IN1", Label: "IN1", Quantity: 3, Unit: "PCS"}, {InstanceID: "IN2", Label: "IN2", Quantity: 4, Unit: "PCS"}}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: append(instances, PackageInstance{
		InstanceID: "ROOT", Label: "ROOT", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), SourceEventID: "e",
	}), Stows: stows, Operations: ops})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sum := m.ContentsAt("ULD-9", now.Add(time.Minute))
	if sum.ItemTotal != 7 || len(sum.LeafPackages) != 2 {
		t.Fatalf("leaf item total must be 7 over 2 leaves, got %d / %v", sum.ItemTotal, sum.LeafPackages)
	}
	if sum.PackageCount != 3 {
		t.Fatalf("package count family reports all 3 descendants, got %d", sum.PackageCount)
	}
}

func TestReusedLabelStaysDistinct(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	instances := []PackageInstance{
		{InstanceID: "X-PEK", Label: "DUP-1", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e1"},
		{InstanceID: "X-SHA", Label: "DUP-1", Issuer: "iss", Scope: "SHA", ActiveFrom: now, SourceEventID: "e2"},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: instances})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var found bool
	for _, f := range m.Findings() {
		if f.Code == FindReusedLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("cross-station label reuse must be surfaced as a distinct-instance info finding")
	}
	r := NewInstanceResolver(instances, nil)
	pek := r.Resolve("DUP-1", "PEK", now.Add(time.Minute))
	sha := r.Resolve("DUP-1", "SHA", now.Add(time.Minute))
	if pek.InstanceID != "X-PEK" || sha.InstanceID != "X-SHA" {
		t.Fatalf("scoped resolution must keep reused labels apart, got %+v %+v", pek, sha)
	}
}

func TestMutexPositionAndLegalTransload(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{{InstanceID: "P", Label: "P", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), SourceEventID: "e"}}
	overlap := []StowInterval{
		{StowID: "a", ChildInstanceID: "P", ParentKind: "container", ParentRef: "U1", ValidFrom: now, SourceEventID: "e1"},
		{StowID: "b", ChildInstanceID: "P", ParentKind: "container", ParentRef: "U2", ValidFrom: now.Add(time.Minute), SourceEventID: "e2"},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Stows: overlap})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasFinding(m, FindMutexPosition) {
		t.Fatalf("overlapping open-ended stows must be a mutex-position violation")
	}
	closed := []StowInterval{
		{StowID: "a", ChildInstanceID: "P", ParentKind: "container", ParentRef: "U1", ValidFrom: now, ValidTo: ptrTime(now.Add(time.Minute)), HandoverEventID: "h1", SourceEventID: "e1"},
		{StowID: "b", ChildInstanceID: "P", ParentKind: "container", ParentRef: "U2", ValidFrom: now.Add(time.Minute), HandoverEventID: "h2", SourceEventID: "e2"},
	}
	m2, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Stows: closed})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if hasFinding(m2, FindMutexPosition) {
		t.Fatalf("sequential transload closed by a handover interval must be legal")
	}
}

func TestTimeInversionAndSelfReflow(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "S", Label: "S", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-3 * time.Hour), SourceEventID: "e"},
		{InstanceID: "M", Label: "M", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), SourceEventID: "e"},
		{InstanceID: "C", Label: "C", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
	}
	ops := []OperationRecord{
		{OperationKey: "OP-1", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-2 * time.Hour),
			ReceivedAt: now.Add(-2 * time.Hour), SourceEventID: "e1", IdempotencyKey: "1", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "S", Label: "S", Quantity: 10, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "M", Label: "M", Quantity: 10, Unit: "PCS"}}},
		// OP-2 happens BEFORE OP-1 but consumes M produced by OP-1 -> inversion.
		{OperationKey: "OP-2", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-3 * time.Hour),
			ReceivedAt: now.Add(-time.Hour), SourceEventID: "e2", IdempotencyKey: "2", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "M", Label: "M", Quantity: 10, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "C", Label: "C", Quantity: 10, Unit: "PCS"}}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: ops})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasFinding(m, FindTimeInversion) {
		t.Fatalf("consumer predating its producer must be a time inversion")
	}

	// Self-reflow: an output instance also listed as an input.
	self := []OperationRecord{{
		OperationKey: "OP-SELF", Version: 1, Kind: OpMixedRepack, Station: "PEK", OccurredAt: now,
		ReceivedAt: now, SourceEventID: "es", IdempotencyKey: "s", Unit: "PCS",
		Inputs:  []PackageQty{{InstanceID: "P", Label: "P", Quantity: 5, Unit: "PCS"}},
		Outputs: []PackageQty{{InstanceID: "P", Label: "P", Quantity: 5, Unit: "PCS"}},
	}}
	m2, err := BuildModel(ModelInputs{Units: reg,
		Instances:  []PackageInstance{{InstanceID: "P", Label: "P", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"}},
		Operations: self})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasFinding(m2, FindSelfReflow) {
		t.Fatalf("package flowing into itself must be flagged")
	}
}

func TestWeightTareReasoning(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "OLD", Label: "OLD", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), ActiveTo: ptrTime(now), SourceEventID: "e"},
		{InstanceID: "NEW", Label: "NEW", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
	}
	op := OperationRecord{
		OperationKey: "OP-RB", Version: 1, Kind: OpRelabel, Station: "PEK", OccurredAt: now,
		ReceivedAt: now, SourceEventID: "e", IdempotencyKey: "rb",
		Inputs: []PackageQty{{InstanceID: "OLD", Label: "OLD", Quantity: 1, Unit: "PCS",
			GrossWeight: &WeightMeasure{Value: 10.0, Unit: "KG", Basis: "gross", Tolerance: 0.2},
			TareWeight:  &WeightMeasure{Value: 1.0, Unit: "KG", Basis: "tare"}}},
		Outputs: []PackageQty{{InstanceID: "NEW", Label: "NEW", Quantity: 1, Unit: "PCS",
			GrossWeight: &WeightMeasure{Value: 10.9, Unit: "KG", Basis: "gross", Tolerance: 0.2},
			TareWeight:  &WeightMeasure{Value: 2.0, Unit: "KG", Basis: "tare"}}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Net 9.0 before vs 8.9 after: within 0.2 tolerance despite different gross.
	var wc WeightConclusion
	for _, w := range m.WeightConclusions() {
		if w.InstanceID == "NEW" {
			wc = w
		}
	}
	if wc.Conclusion != "within_tolerance" {
		t.Fatalf("rebox gross change with known tare must resolve within tolerance, got %+v", wc)
	}

	// Missing tare -> unverifiable, never auto loss.
	op2 := op
	op2.OperationKey = "OP-RB-2"
	op2.Outputs[0].TareWeight = nil
	m2, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: []OperationRecord{op2}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasFinding(m2, FindTareUnverifiable) {
		t.Fatalf("gross change without tare must stay unverifiable")
	}
}

func TestUnconvertibleUnitStaysPending(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "S", Label: "S", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
		{InstanceID: "C", Label: "C", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
	}
	op := OperationRecord{
		OperationKey: "OP-U", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now,
		ReceivedAt: now, SourceEventID: "e", IdempotencyKey: "u", Unit: "PCS",
		Inputs:  []PackageQty{{InstanceID: "S", Label: "S", Quantity: 4, Unit: "PALLET"}},
		Outputs: []PackageQty{{InstanceID: "C", Label: "C", Quantity: 4, Unit: "PCS"}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasFinding(m, FindUnitUnconvertible) {
		t.Fatalf("unknown unit must be held pending, not silently balanced")
	}
	for _, b := range m.Balances() {
		if !b.Pending {
			t.Fatalf("balance with an unconvertible side must be pending verification")
		}
	}
}

func TestPackageItemNettingRejected(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "S", Label: "S", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
		{InstanceID: "C", Label: "C", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
	}
	op := OperationRecord{
		OperationKey: "OP-N", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now,
		ReceivedAt: now, SourceEventID: "e", IdempotencyKey: "n", Unit: "PCS",
		Inputs:  []PackageQty{{InstanceID: "S", Label: "S", Quantity: 2, Unit: "PACKAGE"}},
		Outputs: []PackageQty{{InstanceID: "C", Label: "C", Quantity: 2, Unit: "PCS"}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasFinding(m, FindPackageItemNetting) {
		t.Fatalf("netting package counts against inner-item quantities must be rejected")
	}
}

func TestLateScanAfterSplitNotReplicated(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "DAD", Label: "DAD", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), ActiveTo: ptrTime(now), SourceEventID: "e"},
		{InstanceID: "K1", Label: "K1", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
		{InstanceID: "K2", Label: "K2", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
	}
	op := OperationRecord{
		OperationKey: "OP-SPL", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now,
		ReceivedAt: now, SourceEventID: "e", IdempotencyKey: "spl", Unit: "PCS",
		Inputs:  []PackageQty{{InstanceID: "DAD", Label: "DAD", Quantity: 10, Unit: "PCS"}},
		Outputs: []PackageQty{{InstanceID: "K1", Label: "K1", Quantity: 5, Unit: "PCS"}, {InstanceID: "K2", Label: "K2", Quantity: 5, Unit: "PCS"}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	resolver := NewInstanceResolver(ins, nil)
	// Late scan still naming the parent, no content scope.
	att := m.AttributeScan(*resolver, ScanAttributionRequest{
		EventID: "scan-late", Label: "DAD", Station: "PEK", OccurredAt: now.Add(time.Minute),
	})
	if att.Status != ScanUnattributable {
		t.Fatalf("post-split parent scan without scope must stay unattributable, got %+v", att)
	}
	// An explicit scope binds exactly the named child.
	att2 := m.AttributeScan(*resolver, ScanAttributionRequest{
		EventID: "scan-ok", Label: "K1", Station: "PEK", OccurredAt: now.Add(2 * time.Minute),
		ContentScope: []string{"K1"},
	})
	if att2.Status != ScanAttributed || att2.ResolvedInstance != "K1" {
		t.Fatalf("scoped child scan must attribute to K1, got %+v", att2)
	}
}

func TestRevisionImpactIsDependencyScoped(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "S1", Label: "S1", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-3 * time.Hour), SourceEventID: "e"},
		{InstanceID: "M1", Label: "M1", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), SourceEventID: "e"},
		{InstanceID: "C1", Label: "C1", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
		{InstanceID: "Z1", Label: "Z1", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-3 * time.Hour), SourceEventID: "e"},
	}
	ops := []OperationRecord{
		{OperationKey: "OP-A", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-2 * time.Hour),
			ReceivedAt: now.Add(-2 * time.Hour), SourceEventID: "ea", IdempotencyKey: "a", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "S1", Label: "S1", Quantity: 6, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "M1", Label: "M1", Quantity: 6, Unit: "PCS"}}},
		{OperationKey: "OP-B", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-time.Hour),
			ReceivedAt: now.Add(-time.Hour), SourceEventID: "eb", IdempotencyKey: "b", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "M1", Label: "M1", Quantity: 6, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "C1", Label: "C1", Quantity: 6, Unit: "PCS"}}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: ops})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	affected := m.DownstreamOperations("OP-A")
	want := map[string]bool{"OP-A": true, "OP-B": true}
	if len(affected) != 2 || !want[affected[0]] || !want[affected[1]] {
		t.Fatalf("OP-A correction must reach OP-B, got %v", affected)
	}
	calcs := []CalculationRecord{
		{CalcID: "CALC-1", Basis: []CalcBasis{{OperationKey: "OP-A", Version: 1}, {OperationKey: "OP-B", Version: 1}}},
		{CalcID: "CALC-2", Basis: []CalcBasis{{OperationKey: "OP-OTHER", Version: 1}}},
	}
	imp := ComputeRevisionImpact("OP-A", 2, affected, calcs, map[string]string{"SNAP-1": "CALC-1"})
	if len(imp.InvalidatedCalcIDs) != 1 || imp.InvalidatedCalcIDs[0] != "CALC-1" {
		t.Fatalf("only the dependent calc invalidates, got %v", imp.InvalidatedCalcIDs)
	}
	if len(imp.UnrelatedCalcIDs) != 1 || imp.UnrelatedCalcIDs[0] != "CALC-2" {
		t.Fatalf("unrelated calc must remain valid, got %v", imp.UnrelatedCalcIDs)
	}
	if len(imp.RecomputableSnapshots) != 1 || imp.RecomputableSnapshots[0] != "SNAP-1" {
		t.Fatalf("existing publication must stay recomputable on its old basis, got %v", imp.RecomputableSnapshots)
	}
}

func TestDeepChainRangesAcrossOperations(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "S", Label: "S", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-3 * time.Hour), SourceEventID: "e"},
		{InstanceID: "M", Label: "M", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-2 * time.Hour), SourceEventID: "e"},
		{InstanceID: "C1", Label: "C1", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
		{InstanceID: "C2", Label: "C2", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
	}
	ops := []OperationRecord{
		{OperationKey: "OP-1", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-2 * time.Hour),
			ReceivedAt: now.Add(-2 * time.Hour), SourceEventID: "e1", IdempotencyKey: "1", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "S", Label: "S", Quantity: 10, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "M", Label: "M", Quantity: 10, Unit: "PCS"}}},
		{OperationKey: "OP-2", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now.Add(-time.Hour),
			ReceivedAt: now.Add(-time.Hour), SourceEventID: "e2", IdempotencyKey: "2", Unit: "PCS",
			Inputs:  []PackageQty{{InstanceID: "M", Label: "M", Quantity: 10, Unit: "PCS"}},
			Outputs: []PackageQty{{InstanceID: "C1", Label: "C1", Quantity: 6, Unit: "PCS"}, {InstanceID: "C2", Label: "C2", Quantity: 4, Unit: "PCS"}}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: ops})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r1 := m.SourceRange(SourceRangeQuery{SourceInstanceID: "S", TargetInstanceID: "C1"})
	if r1.Lower != 6 || r1.Upper != 6 || r1.Status != "exact" {
		t.Fatalf("deterministic chain S->C1 must be exact 6, got %s [%d,%d]", r1.Status, r1.Lower, r1.Upper)
	}
	r2 := m.SourceRange(SourceRangeQuery{SourceInstanceID: "S", TargetInstanceID: "C2"})
	if r2.Lower != 4 || r2.Upper != 4 {
		t.Fatalf("S->C2 must be exact 4, got [%d,%d]", r2.Lower, r2.Upper)
	}
}

func TestDozenConversionIsIntegral(t *testing.T) {
	now := pedigreeBaseTime()
	reg := testRegistry(t)
	ins := []PackageInstance{
		{InstanceID: "S", Label: "S", Issuer: "iss", Scope: "PEK", ActiveFrom: now.Add(-time.Hour), SourceEventID: "e"},
		{InstanceID: "C", Label: "C", Issuer: "iss", Scope: "PEK", ActiveFrom: now, SourceEventID: "e"},
	}
	op := OperationRecord{
		OperationKey: "OP-D", Version: 1, Kind: OpSplit, Station: "PEK", OccurredAt: now,
		ReceivedAt: now, SourceEventID: "e", IdempotencyKey: "d", Unit: "PCS",
		Inputs:  []PackageQty{{InstanceID: "S", Label: "S", Quantity: 1, Unit: "DOZEN"}}, // 12 PCS
		Outputs: []PackageQty{{InstanceID: "C", Label: "C", Quantity: 12, Unit: "PCS"}},
	}
	m, err := BuildModel(ModelInputs{Units: reg, Instances: ins, Operations: []OperationRecord{op}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if hasFinding(m, FindConservationMismatch) || hasFinding(m, FindUnitUnconvertible) {
		t.Fatalf("1 dozen = 12 PCS must reconcile exactly")
	}
	r := m.SourceRange(SourceRangeQuery{SourceInstanceID: "S", TargetInstanceID: "C"})
	if r.Status != "exact" || r.Lower != 12 {
		t.Fatalf("converted range must be exact 12 base units, got %+v", r)
	}
}

func hasFinding(m *PedigreeModel, code string) bool {
	for _, f := range m.Findings() {
		if f.Code == code {
			return true
		}
	}
	return false
}

func ptrTime(t time.Time) *time.Time { return &t }
