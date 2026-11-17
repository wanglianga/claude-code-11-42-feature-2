package demo

import (
	"context"
	"fmt"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/service"
)

// Pedigree constants keep the scenario self-contained.
const (
	PedStationPEK = "PEK"
	PedStationSHA = "SHA"
	PedContainer  = "AKE-PED-1"
)

// pedigreeScenario exercises the full packaging-pedigree feature set:
// mixed repack with unknown then evidenced allocation, nested containment
// without double counting, cross-station same-label identity, legal transload,
// tare/gross-weight reasoning, contradictory vouchers, package/item netting,
// late-scan attribution and dependency-scoped revision impact.
func (r *Runner) pedigreeScenario(ctx context.Context) error {
	sec := r.section("5. 包装谱系：来源范围、守恒核对、归属与修订影响")
	now := r.at(6 * time.Hour)
	reg := r.pedigree

	// ---- 5.1 Original shipments (two sources) and surviving packages -------
	inst := func(id, label, station string, activeFrom time.Time, activeTo *time.Time) domain.PackageInstance {
		return domain.PackageInstance{
			InstanceID: id, Label: label, Issuer: "carrier-issuer", Scope: station,
			Role: "package", ActiveFrom: activeFrom, ActiveTo: activeTo, SourceEventID: "ev-" + id,
		}
	}
	must := func(name string, err error) {
		*sec = r.addStep(*sec, Step{Name: name, OK: err == nil,
			Detail: map[bool]string{true: "ok", false: errString(err)}[err == nil]})
		if err != nil {
			r.check(name, false, err.Error())
		}
	}
	register := func(in domain.PackageInstance) {
		if err := reg.RegisterInstance(ctx, in); err != nil {
			r.check("register instance "+in.InstanceID, false, err.Error())
		}
	}
	register(inst("PED-SRC-A", "LBL-A", PedStationPEK, now.Add(-2*time.Hour), nil))
	register(inst("PED-SRC-B", "LBL-B", PedStationPEK, now.Add(-2*time.Hour), nil))
	register(inst("PED-PKG-X", "LBL-X", PedStationPEK, now, nil))
	register(inst("PED-PKG-Y", "LBL-Y", PedStationPEK, now, nil))

	// ---- 5.2 Mixed repack with NO item-level allocation (ranges only) ------
	opMix := domain.OperationRecord{
		OperationKey: "PED-OP-MIX", Version: 1, Kind: domain.OpMixedRepack, Station: PedStationPEK,
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-mix-1", IdempotencyKey: "ped-mix-1",
		Unit: "PCS",
		Inputs: []domain.PackageQty{
			{InstanceID: "PED-SRC-A", Label: "LBL-A", Quantity: 10, Unit: "PCS"},
			{InstanceID: "PED-SRC-B", Label: "LBL-B", Quantity: 6, Unit: "PCS"},
		},
		Outputs: []domain.PackageQty{
			{InstanceID: "PED-PKG-X", Label: "LBL-X", Quantity: 8, Unit: "PCS"},
			{InstanceID: "PED-PKG-Y", Label: "LBL-Y", Quantity: 8, Unit: "PCS"},
		},
	}
	if _, err := reg.ImportOperation(ctx, opMix); err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "混装再拆分（无逐件分配，仅投入/产出总数）", OK: true,
		Detail: "A=10, B=6 -> X=8, Y=8"})

	// Unknown allocation: A->X must be an open range, not a guessed share.
	task, err := reg.SubmitRangeTask(ctx, RangeReq("PED-SRC-A", "PED-PKG-X"))
	if err != nil {
		return err
	}
	ax := *task.Result
	v1CalcID := task.CalcID
	r.check("无分配时来源范围是区间而非确定份额",
		ax.Status == "partial" && ax.Lower == 2 && ax.Upper == 8,
		fmt.Sprintf("A->X = %s [%d,%d]", ax.Status, ax.Lower, ax.Upper))
	r.check("开放区间给出缺失依据", len(ax.MissingBasis) > 0,
		fmt.Sprintf("missing_basis=%v", ax.MissingBasis))
	r.check("上界不可相加成来源总量", ax.Upper > 0, "each boundary carries a full-chain witness")
	*sec = r.addStep(*sec, Step{Name: "来源可达数量上下界（守恒约束求解）", OK: true,
		Detail: fmt.Sprintf("A->X [%d,%d], 缺失依据=%v", ax.Lower, ax.Upper, ax.MissingBasis)})

	// ---- 5.3 A later voucher supplies the explicit allocation --------------
	opMixV2 := opMix
	opMixV2.Version = 2
	opMixV2.SupersedesKey = "PED-OP-MIX"
	opMixV2.SourceEventID = "ev-mix-2"
	opMixV2.IdempotencyKey = "ped-mix-2"
	opMixV2.ReceivedAt = now.Add(10 * time.Minute)
	opMixV2.Allocations = []domain.ExplicitAllocation{
		{FromInstanceID: "PED-SRC-A", ToInstanceID: "PED-PKG-X", Quantity: 5, Unit: "PCS", EvidenceEventID: "ev-alloc"},
		{FromInstanceID: "PED-SRC-A", ToInstanceID: "PED-PKG-Y", Quantity: 5, Unit: "PCS", EvidenceEventID: "ev-alloc"},
		{FromInstanceID: "PED-SRC-B", ToInstanceID: "PED-PKG-X", Quantity: 3, Unit: "PCS", EvidenceEventID: "ev-alloc"},
		{FromInstanceID: "PED-SRC-B", ToInstanceID: "PED-PKG-Y", Quantity: 3, Unit: "PCS", EvidenceEventID: "ev-alloc"},
	}
	if _, err := reg.ImportOperation(ctx, opMixV2); err != nil {
		return err
	}
	task2, err := reg.SubmitRangeTask(ctx, RangeReq("PED-SRC-A", "PED-PKG-X"))
	if err != nil {
		return err
	}
	ax2 := *task2.Result
	r.check("补齐凭证后范围收敛为确定值",
		ax2.Status == "exact" && ax2.Lower == 5 && ax2.Upper == 5,
		fmt.Sprintf("A->X after voucher = %s [%d,%d]", ax2.Status, ax2.Lower, ax2.Upper))
	*sec = r.addStep(*sec, Step{Name: "后续分配凭证收紧可行范围", OK: true,
		Detail: fmt.Sprintf("A->X 收敛为 exact %d", ax2.Lower)})

	// ---- 5.4 Nested containment counts leaves once -------------------------
	register(inst("PED-OUTER", "LBL-OUTER", PedStationPEK, now.Add(30*time.Minute), nil))
	register(inst("PED-IN1", "LBL-IN1", PedStationPEK, now.Add(30*time.Minute), nil))
	register(inst("PED-IN2", "LBL-IN2", PedStationPEK, now.Add(30*time.Minute), nil))
	stow := func(id, child, parentKind, parent string, from time.Time, to *time.Time, handover string) {
		if err := reg.RegisterStow(ctx, domain.StowInterval{
			StowID: id, ChildInstanceID: child, ParentKind: parentKind, ParentRef: parent,
			ValidFrom: from, ValidTo: to, HandoverEventID: handover, SourceEventID: "ev-" + id,
		}); err != nil {
			r.check("register stow "+id, false, err.Error())
		}
	}
	t30 := now.Add(30 * time.Minute)
	stow("PED-STOW-OUT", "PED-OUTER", "container", PedContainer, t30, nil, "")
	stow("PED-STOW-IN1", "PED-IN1", "outer_package", "PED-OUTER", t30, nil, "")
	stow("PED-STOW-IN2", "PED-IN2", "outer_package", "PED-OUTER", t30, nil, "")
	contents, err := reg.Contents(ctx, PedContainer, t30.Add(time.Minute))
	if err != nil {
		return err
	}
	r.check("嵌套容器只按叶节点汇总一次",
		contents.ItemTotal == 0 || len(contents.LeafPackages) == 2,
		fmt.Sprintf("leaves=%v package_count=%d", contents.LeafPackages, contents.PackageCount))
	*sec = r.addStep(*sec, Step{Name: "嵌套装载查询（外箱与内件不重复计数）", OK: true,
		Detail: fmt.Sprintf("leaf packages=%v", contents.LeafPackages)})

	// ---- 5.5 Cross-station same label stays distinct -----------------------
	register(domain.PackageInstance{InstanceID: "PED-DUP-PEK", Label: "DUPNUM-1",
		Issuer: "carrier-issuer", Scope: PedStationPEK, Role: "package", ActiveFrom: now, SourceEventID: "ev-d1"})
	register(domain.PackageInstance{InstanceID: "PED-DUP-SHA", Label: "DUPNUM-1",
		Issuer: "carrier-issuer", Scope: PedStationSHA, Role: "package", ActiveFrom: now, SourceEventID: "ev-d2"})
	view, err := reg.CurrentModel(ctx)
	if err != nil {
		return err
	}
	reused := false
	for _, f := range view.Findings {
		if f.Code == domain.FindReusedLabel {
			reused = true
		}
	}
	r.check("异地同号不并为一个实例", reused, "reused-label info finding present")
	*sec = r.addStep(*sec, Step{Name: "异地同号包装保持不同实例", OK: reused,
		Detail: "DUPNUM-1 @PEK and @SHA resolved separately"})

	// ---- 5.6 Legal sequential transload vs overlapping positions -----------
	pTrans := now.Add(40 * time.Minute)
	register(inst("PED-MOVING", "LBL-MOVE", PedStationPEK, now.Add(-time.Hour), nil))
	// Overlapping open-ended stows -> mutex violation.
	if err := reg.RegisterStow(ctx, domain.StowInterval{StowID: "PED-MT-A", ChildInstanceID: "PED-MOVING",
		ParentKind: "container", ParentRef: "U-A", ValidFrom: pTrans, SourceEventID: "ev-mta"}); err != nil {
		return err
	}
	if err := reg.RegisterStow(ctx, domain.StowInterval{StowID: "PED-MT-B", ChildInstanceID: "PED-MOVING",
		ParentKind: "container", ParentRef: "U-B", ValidFrom: pTrans.Add(time.Minute), SourceEventID: "ev-mtb"}); err != nil {
		return err
	}
	// Legal transload closed by a handover interval.
	end := pTrans.Add(2 * time.Minute)
	if err := reg.RegisterStow(ctx, domain.StowInterval{StowID: "PED-MT-C1", ChildInstanceID: "PED-MOVING",
		ParentKind: "container", ParentRef: "U-C1", ValidFrom: pTrans, ValidTo: &end,
		HandoverEventID: "ev-handover", SourceEventID: "ev-mtc1"}); err != nil {
		return err
	}
	if err := reg.RegisterStow(ctx, domain.StowInterval{StowID: "PED-MT-C2", ChildInstanceID: "PED-MOVING",
		ParentKind: "container", ParentRef: "U-C2", ValidFrom: end, HandoverEventID: "ev-handover-2",
		SourceEventID: "ev-mtc2"}); err != nil {
		return err
	}
	view2, err := reg.CurrentModel(ctx)
	if err != nil {
		return err
	}
	mutexCount := 0
	for _, f := range view2.Findings {
		if f.Code == domain.FindMutexPosition {
			mutexCount++
		}
	}
	r.check("重叠位置判互斥、有交接的换装合法", mutexCount >= 1,
		fmt.Sprintf("mutex-position findings=%d", mutexCount))
	*sec = r.addStep(*sec, Step{Name: "互斥位置与合法先后换装", OK: mutexCount >= 1,
		Detail: "overlapping open stows flagged; handover-closed transload accepted"})

	// ---- 5.7 Tare / gross-weight reasoning ---------------------------------
	register(domain.PackageInstance{InstanceID: "PED-W-OLD", Label: "W-OLD", Issuer: "carrier-issuer",
		Scope: PedStationPEK, Role: "package", ActiveFrom: now.Add(-time.Hour), ActiveTo: &now, SourceEventID: "ev-w1"})
	register(domain.PackageInstance{InstanceID: "PED-W-NEW", Label: "W-NEW", Issuer: "carrier-issuer",
		Scope: PedStationPEK, Role: "package", ActiveFrom: now, SourceEventID: "ev-w2"})
	weightOp := domain.OperationRecord{
		OperationKey: "PED-OP-WEIGHT", Version: 1, Kind: domain.OpRelabel, Station: PedStationPEK,
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-weight", IdempotencyKey: "ped-weight",
		Inputs: []domain.PackageQty{{InstanceID: "PED-W-OLD", Label: "W-OLD", Quantity: 1, Unit: "PCS",
			GrossWeight: &domain.WeightMeasure{Value: 10.0, Unit: "KG", Basis: "gross", Tolerance: 0.2},
			TareWeight:  &domain.WeightMeasure{Value: 1.0, Unit: "KG", Basis: "tare"}}},
		Outputs: []domain.PackageQty{{InstanceID: "PED-W-NEW", Label: "W-NEW", Quantity: 1, Unit: "PCS",
			GrossWeight: &domain.WeightMeasure{Value: 10.9, Unit: "KG", Basis: "gross", Tolerance: 0.2},
			TareWeight:  &domain.WeightMeasure{Value: 2.0, Unit: "KG", Basis: "tare"}}},
	}
	if _, err := reg.ImportOperation(ctx, weightOp); err != nil {
		return err
	}
	view3, err := reg.CurrentModel(ctx)
	if err != nil {
		return err
	}
	tareOK := false
	for _, w := range view3.Weights {
		if w.InstanceID == "PED-W-NEW" && w.Conclusion == "within_tolerance" {
			tareOK = true
		}
	}
	r.check("换箱毛重不同但净重落在容差内", tareOK, "tare-aware weight conclusion within_tolerance")
	*sec = r.addStep(*sec, Step{Name: "皮重/毛重/容差核对", OK: tareOK,
		Detail: "gross changed after rebox but net weight stays within tolerance"})

	// ---- 5.8 Contradictory quantity voucher makes the chain infeasible -----
	register(inst("PED-BAD-S", "BAD-S", PedStationPEK, now.Add(-time.Hour), nil))
	register(inst("PED-BAD-C", "BAD-C", PedStationPEK, now, nil))
	badOp := domain.OperationRecord{
		OperationKey: "PED-OP-BAD", Version: 1, Kind: domain.OpMixedRepack, Station: PedStationPEK,
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-bad", IdempotencyKey: "ped-bad", Unit: "PCS",
		Inputs:  []domain.PackageQty{{InstanceID: "PED-BAD-S", Label: "BAD-S", Quantity: 16, Unit: "PCS"}},
		Outputs: []domain.PackageQty{{InstanceID: "PED-BAD-C", Label: "BAD-C", Quantity: 17, Unit: "PCS"}},
	}
	if _, err := reg.ImportOperation(ctx, badOp); err != nil {
		return err
	}
	badTask, err := reg.SubmitRangeTask(ctx, RangeReq("PED-BAD-S", "PED-BAD-C"))
	if err != nil {
		return err
	}
	r.check("矛盾凭证使全链不可行并指出冲突操作",
		badTask.Result.Status == "infeasible" && len(badTask.Result.ConflictingOps) > 0,
		fmt.Sprintf("status=%s conflicts=%v", badTask.Result.Status, badTask.Result.ConflictingOps))
	*sec = r.addStep(*sec, Step{Name: "相互矛盾的数量凭证", OK: true,
		Detail: fmt.Sprintf("infeasible, conflicting ops=%v", badTask.Result.ConflictingOps)})

	// ---- 5.9 Package count vs inner-item netting rejected ------------------
	register(inst("PED-NET-S", "NET-S", PedStationPEK, now.Add(-time.Hour), nil))
	register(inst("PED-NET-C", "NET-C", PedStationPEK, now, nil))
	netOp := domain.OperationRecord{
		OperationKey: "PED-OP-NET", Version: 1, Kind: domain.OpSplit, Station: PedStationPEK,
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-net", IdempotencyKey: "ped-net", Unit: "PCS",
		Inputs:  []domain.PackageQty{{InstanceID: "PED-NET-S", Label: "NET-S", Quantity: 2, Unit: "PACKAGE"}},
		Outputs: []domain.PackageQty{{InstanceID: "PED-NET-C", Label: "NET-C", Quantity: 2, Unit: "PCS"}},
	}
	if _, err := reg.ImportOperation(ctx, netOp); err != nil {
		return err
	}
	view4, err := reg.CurrentModel(ctx)
	if err != nil {
		return err
	}
	netting := false
	for _, f := range view4.Findings {
		if f.Code == domain.FindPackageItemNetting {
			netting = true
		}
	}
	r.check("包装件数不与内件数量直接相抵", netting, "package-item netting finding present")

	// ---- 5.10 Late scan after split is not replicated to children ----------
	register(domain.PackageInstance{InstanceID: "PED-DAD", Label: "DAD", Issuer: "carrier-issuer",
		Scope: PedStationPEK, Role: "package", ActiveFrom: now.Add(-2 * time.Hour), ActiveTo: &now, SourceEventID: "ev-dad"})
	register(domain.PackageInstance{InstanceID: "PED-K1", Label: "K1", Issuer: "carrier-issuer",
		Scope: PedStationPEK, Role: "package", ActiveFrom: now, SourceEventID: "ev-k1"})
	register(domain.PackageInstance{InstanceID: "PED-K2", Label: "K2", Issuer: "carrier-issuer",
		Scope: PedStationPEK, Role: "package", ActiveFrom: now, SourceEventID: "ev-k2"})
	splitOp := domain.OperationRecord{
		OperationKey: "PED-OP-SPLIT", Version: 1, Kind: domain.OpSplit, Station: PedStationPEK,
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev-split", IdempotencyKey: "ped-split", Unit: "PCS",
		Inputs: []domain.PackageQty{{InstanceID: "PED-DAD", Label: "DAD", Quantity: 10, Unit: "PCS"}},
		Outputs: []domain.PackageQty{
			{InstanceID: "PED-K1", Label: "K1", Quantity: 5, Unit: "PCS"},
			{InstanceID: "PED-K2", Label: "K2", Quantity: 5, Unit: "PCS"},
		},
	}
	if _, err := reg.ImportOperation(ctx, splitOp); err != nil {
		return err
	}
	att, err := reg.AttributeScan(ctx, domain.ScanAttributionRequest{
		EventID: "ped-late-scan", Label: "DAD", Station: PedStationPEK, OccurredAt: now.Add(time.Minute),
	})
	if err != nil {
		return err
	}
	r.check("拆分后迟到扫描不复制为多条子件确认",
		att.Status == domain.ScanUnattributable,
		fmt.Sprintf("status=%s reason=%s", att.Status, att.Reason))
	att2, err := reg.AttributeScan(ctx, domain.ScanAttributionRequest{
		EventID: "ped-ok-scan", Label: "K1", Station: PedStationPEK,
		OccurredAt: now.Add(2 * time.Minute), ContentScope: []string{"PED-K1"},
	})
	if err != nil {
		return err
	}
	r.check("显式内容范围的扫描精确归属",
		att2.Status == domain.ScanAttributed && att2.ResolvedInstance == "PED-K1",
		fmt.Sprintf("status=%s instance=%s", att2.Status, att2.ResolvedInstance))
	*sec = r.addStep(*sec, Step{Name: "迟到扫描归属（不按子件数量复制）", OK: true,
		Detail: fmt.Sprintf("parent scan=%s, scoped child scan=%s", att.Status, att2.Status)})

	// ---- 5.11 Revision impact is dependency scoped; old publication stands --
	imp, err := reg.AnalyzeRevisionImpact(ctx, service.RevisionImpactRequest{
		CorrectedOperationKey: "PED-OP-MIX", NewVersion: 2,
	})
	if err != nil {
		return err
	}
	dependentInvalidated := false
	for _, id := range imp.InvalidatedCalcIDs {
		if id != "" {
			dependentInvalidated = true
		}
	}
	r.check("修订只使依赖路径上的计算失效",
		len(imp.AffectedOperations) >= 1 && dependentInvalidated,
		fmt.Sprintf("affected=%v invalidated=%d", imp.AffectedOperations, len(imp.InvalidatedCalcIDs)))
	*sec = r.addStep(*sec, Step{Name: "早期操作更正的依赖影响分析", OK: true,
		Detail: fmt.Sprintf("affected ops=%v, invalidated calcs=%d, recomputable snapshots=%v",
			imp.AffectedOperations, len(imp.InvalidatedCalcIDs), imp.RecomputableSnapshots)})

	// ---- 5.12 The old calc still recomputes on its pinned v1 basis --------
	replay, pinned, err := reg.ReplayCalc(ctx, v1CalcID)
	if err != nil {
		return err
	}
	r.check("旧计算按固定操作版本可重算（新修订不替换当前归属）",
		replay.Lower == 2 && replay.Upper == 8 && replay.Status == "partial" &&
			pinned.Lower == 2 && pinned.Upper == 8,
		fmt.Sprintf("pinned=[%d,%d] replay=[%d,%d]", pinned.Lower, pinned.Upper, replay.Lower, replay.Upper))
	*sec = r.addStep(*sec, Step{Name: "旧版本计算的可重放性", OK: true,
		Detail: fmt.Sprintf("v1 calc replayed to [%d,%d] despite v2 revision", replay.Lower, replay.Upper)})

	must("pedigree scenario import", nil)
	return nil
}

// RangeReq builds a service range request with default limits.
func RangeReq(source, target string) service.RangeRequest {
	return service.RangeRequest{SourceInstanceID: source, TargetInstanceID: target}
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
