package service

import (
	"context"
	"testing"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/store"
)

// newPedigreeService builds a service against the test database when one is
// reachable; the test skips otherwise (mirrors the api integration tests).
func newPedigreeService(t *testing.T) (*PedigreeService, context.Context) {
	t.Helper()
	dsn := "postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable"
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	pool := st.Pool()
	t.Cleanup(func() { pool.Close() })
	if err := st.Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewPedigreeService(store.NewPedigreeStore(pool)), context.Background()
}

func TestPedigreeRangeNarrowsAfterVoucher(t *testing.T) {
	svc, ctx := newPedigreeService(t)
	now := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	mk := func(id string) domain.PackageInstance {
		return domain.PackageInstance{InstanceID: id, Label: id, Issuer: "iss", Scope: "PEK",
			Role: "package", ActiveFrom: now, SourceEventID: "ev-" + id}
	}
	for _, id := range []string{"SA", "SB", "PX", "PY"} {
		if err := svc.RegisterInstance(ctx, mk(id)); err != nil {
			t.Fatalf("instance %s: %v", id, err)
		}
	}
	op := domain.OperationRecord{
		OperationKey: "OP1", Version: 1, Kind: domain.OpMixedRepack, Station: "PEK",
		OccurredAt: now, ReceivedAt: now, SourceEventID: "ev1", IdempotencyKey: "idem1", Unit: "PCS",
		Inputs: []domain.PackageQty{
			{InstanceID: "SA", Quantity: 10, Unit: "PCS"},
			{InstanceID: "SB", Quantity: 6, Unit: "PCS"},
		},
		Outputs: []domain.PackageQty{
			{InstanceID: "PX", Quantity: 8, Unit: "PCS"},
			{InstanceID: "PY", Quantity: 8, Unit: "PCS"},
		},
	}
	if _, err := svc.ImportOperation(ctx, op); err != nil {
		t.Fatalf("import: %v", err)
	}
	open, err := svc.SubmitRangeTask(ctx, RangeRequest{SourceInstanceID: "SA", TargetInstanceID: "PX"})
	if err != nil {
		t.Fatalf("range task: %v", err)
	}
	if open.Result.Upper != 8 || open.Result.Lower != 2 {
		t.Fatalf("open range want [2,8], got [%d,%d]", open.Result.Lower, open.Result.Upper)
	}
	// Correction pins explicit allocations.
	op.Version = 2
	op.SupersedesKey = "OP1"
	op.SourceEventID = "ev2"
	op.IdempotencyKey = "idem2"
	op.Allocations = []domain.ExplicitAllocation{
		{FromInstanceID: "SA", ToInstanceID: "PX", Quantity: 5, Unit: "PCS", EvidenceEventID: "ev-a"},
		{FromInstanceID: "SA", ToInstanceID: "PY", Quantity: 5, Unit: "PCS", EvidenceEventID: "ev-a"},
		{FromInstanceID: "SB", ToInstanceID: "PX", Quantity: 3, Unit: "PCS", EvidenceEventID: "ev-a"},
		{FromInstanceID: "SB", ToInstanceID: "PY", Quantity: 3, Unit: "PCS", EvidenceEventID: "ev-a"},
	}
	if _, err := svc.ImportOperation(ctx, op); err != nil {
		t.Fatalf("import v2: %v", err)
	}
	pinned, err := svc.SubmitRangeTask(ctx, RangeRequest{SourceInstanceID: "SA", TargetInstanceID: "PX"})
	if err != nil {
		t.Fatalf("pinned range: %v", err)
	}
	if pinned.Result.Status != "exact" || pinned.Result.Lower != 5 || pinned.Result.Upper != 5 {
		t.Fatalf("pinned range want exact 5, got %s [%d,%d]", pinned.Result.Status, pinned.Result.Lower, pinned.Result.Upper)
	}
	// The old calc still replays on its v1 basis.
	replay, _, err := svc.ReplayCalc(ctx, open.CalcID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Lower != 2 || replay.Upper != 8 {
		t.Fatalf("replay want [2,8], got [%d,%d]", replay.Lower, replay.Upper)
	}
}

func TestPedigreeScanNotReplicatedAfterSplit(t *testing.T) {
	svc, ctx := newPedigreeService(t)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	end := now
	inst := func(id string, activeFrom time.Time, activeTo *time.Time) domain.PackageInstance {
		return domain.PackageInstance{InstanceID: id, Label: id, Issuer: "iss", Scope: "PEK",
			Role: "package", ActiveFrom: activeFrom, ActiveTo: activeTo, SourceEventID: "ev-" + id}
	}
	for _, spec := range []struct {
		id   string
		from time.Time
		to   *time.Time
	}{
		{"DAD", now.Add(-time.Hour), &end}, {"K1", now, nil}, {"K2", now, nil},
	} {
		if err := svc.RegisterInstance(ctx, inst(spec.id, spec.from, spec.to)); err != nil {
			t.Fatalf("instance: %v", err)
		}
	}
	if _, err := svc.ImportOperation(ctx, domain.OperationRecord{
		OperationKey: "OPS", Version: 1, Kind: domain.OpSplit, Station: "PEK",
		OccurredAt: now, ReceivedAt: now, SourceEventID: "evs", IdempotencyKey: "idems", Unit: "PCS",
		Inputs: []domain.PackageQty{{InstanceID: "DAD", Quantity: 10, Unit: "PCS"}},
		Outputs: []domain.PackageQty{
			{InstanceID: "K1", Quantity: 5, Unit: "PCS"},
			{InstanceID: "K2", Quantity: 5, Unit: "PCS"},
		},
	}); err != nil {
		t.Fatalf("import split: %v", err)
	}
	att, err := svc.AttributeScan(ctx, domain.ScanAttributionRequest{
		EventID: "scan1", Label: "DAD", Station: "PEK", OccurredAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if att.Status != domain.ScanUnattributable || len(att.EligibleTargets) != 0 {
		t.Fatalf("late parent scan must be unattributable with no children, got %s targets=%v",
			att.Status, att.EligibleTargets)
	}
}
