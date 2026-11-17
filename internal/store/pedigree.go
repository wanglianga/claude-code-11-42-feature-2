package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cargo.example/secstate/internal/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PedigreeStore persists the packaging-pedigree tables. Every write is
// append-only; corrections create new operation versions and revisions
// supersede calculations rather than overwriting them.
type PedigreeStore struct {
	pool *pgxpool.Pool
}

// NewPedigreeStore builds the store.
func NewPedigreeStore(pool *pgxpool.Pool) *PedigreeStore { return &PedigreeStore{pool: pool} }

// ErrDuplicatePedigreeKey is returned on an idempotency/unique conflict.
var ErrDuplicatePedigreeKey = errors.New("duplicate pedigree key")

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- Instances -------------------------------------------------------------

// InsertInstance registers a package instance.
func (s *PedigreeStore) InsertInstance(ctx context.Context, in domain.PackageInstance) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pedigree_instances
		  (instance_id, label, issuer, scope, revision, role, active_from, active_to, source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (instance_id) DO NOTHING`,
		in.InstanceID, in.Label, in.Issuer, in.Scope, nullable(in.Revision), in.Role,
		in.ActiveFrom, nullableTime(in.ActiveTo), nullable(in.SourceEventID))
	if err != nil {
		return fmt.Errorf("insert pedigree instance: %w", err)
	}
	return nil
}

// ListInstances returns every registered instance.
func (s *PedigreeStore) ListInstances(ctx context.Context) ([]domain.PackageInstance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT instance_id, label, issuer, scope, COALESCE(revision,''), role,
		       active_from, active_to, COALESCE(source_event_id::text,'')
		FROM pedigree_instances ORDER BY instance_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PackageInstance
	for rows.Next() {
		var in domain.PackageInstance
		if err := rows.Scan(&in.InstanceID, &in.Label, &in.Issuer, &in.Scope, &in.Revision,
			&in.Role, &in.ActiveFrom, &in.ActiveTo, &in.SourceEventID); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ---- Aliases ---------------------------------------------------------------

func (s *PedigreeStore) InsertAlias(ctx context.Context, a domain.AliasRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pedigree_aliases
		  (alias_id, from_instance_id, to_instance_id, effective_at, basis_event_id, source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (alias_id) DO NOTHING`,
		a.AliasID, a.FromInstanceID, a.ToInstanceID, a.EffectiveAt, a.BasisEventID, a.SourceEventID)
	if err != nil {
		return fmt.Errorf("insert pedigree alias: %w", err)
	}
	return nil
}

func (s *PedigreeStore) ListAliases(ctx context.Context) ([]domain.AliasRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT alias_id, from_instance_id, to_instance_id, effective_at, basis_event_id, source_event_id
		FROM pedigree_aliases ORDER BY alias_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AliasRecord
	for rows.Next() {
		var a domain.AliasRecord
		if err := rows.Scan(&a.AliasID, &a.FromInstanceID, &a.ToInstanceID, &a.EffectiveAt,
			&a.BasisEventID, &a.SourceEventID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- Stows -----------------------------------------------------------------

func (s *PedigreeStore) InsertStow(ctx context.Context, st domain.StowInterval) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pedigree_stows
		  (stow_id, child_instance_id, parent_kind, parent_ref, valid_from, valid_to,
		   handover_event_id, source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (stow_id) DO NOTHING`,
		st.StowID, st.ChildInstanceID, st.ParentKind, st.ParentRef, st.ValidFrom,
		nullableTime(st.ValidTo), nullable(st.HandoverEventID), nullable(st.SourceEventID))
	if err != nil {
		return fmt.Errorf("insert pedigree stow: %w", err)
	}
	return nil
}

func (s *PedigreeStore) ListStows(ctx context.Context) ([]domain.StowInterval, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT stow_id, child_instance_id, parent_kind, parent_ref, valid_from, valid_to,
		       COALESCE(handover_event_id::text,''), COALESCE(source_event_id::text,'')
		FROM pedigree_stows ORDER BY stow_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.StowInterval
	for rows.Next() {
		var st domain.StowInterval
		if err := rows.Scan(&st.StowID, &st.ChildInstanceID, &st.ParentKind, &st.ParentRef,
			&st.ValidFrom, &st.ValidTo, &st.HandoverEventID, &st.SourceEventID); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---- Operations (versioned, append-only) -----------------------------------

func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }

// InsertOperation appends a new operation version. A repeated idempotency key
// returns the existing row instead of creating a duplicate.
func (s *PedigreeStore) InsertOperation(ctx context.Context, op domain.OperationRecord) (bool, error) {
	inputs, _ := marshalJSON(op.Inputs)
	outputs, _ := marshalJSON(op.Outputs)
	allocs, _ := marshalJSON(op.Allocations)
	losses, _ := marshalJSON(op.Losses)
	raw, _ := marshalJSON(op.RawPayload)
	var existingID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO pedigree_operations
		  (operation_key, version, supersedes_key, kind, station, occurred_at, received_at,
		   source_event_id, idempotency_key, unit, container_id, handover_event_id,
		   inputs, outputs, allocations, losses, raw_payload, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (operation_key, version) DO NOTHING
		RETURNING operation_key`,
		op.OperationKey, op.Version, nullable(op.SupersedesKey), op.Kind, op.Station,
		op.OccurredAt, op.ReceivedAt, op.SourceEventID, op.IdempotencyKey, nullable(op.Unit),
		nullable(op.ContainerID), nullable(op.HandoverEventID),
		inputs, outputs, allocs, losses, raw, nullable(op.Note)).Scan(&existingID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Version conflict: the row already exists. Treat as idempotent no-op.
		return false, nil
	}
	if err != nil {
		if isUniqueViolation(err) {
			return false, ErrDuplicatePedigreeKey
		}
		return false, fmt.Errorf("insert pedigree operation: %w", err)
	}
	return true, nil
}

func scanOperation(row pgx.Row) (domain.OperationRecord, error) {
	var op domain.OperationRecord
	var inputs, outputs, allocs, losses, raw []byte
	var supersedes, unit, container, handover, note *string
	err := row.Scan(
		&op.OperationKey, &op.Version, &supersedes, &op.Kind, &op.Station, &op.OccurredAt,
		&op.ReceivedAt, &op.SourceEventID, &op.IdempotencyKey, &unit, &container, &handover,
		&inputs, &outputs, &allocs, &losses, &raw, &note)
	if err != nil {
		return op, err
	}
	op.SupersedesKey = deref(supersedes)
	op.Unit = deref(unit)
	op.ContainerID = deref(container)
	op.HandoverEventID = deref(handover)
	op.Note = deref(note)
	if err := json.Unmarshal(inputs, &op.Inputs); err != nil {
		return op, err
	}
	if err := json.Unmarshal(outputs, &op.Outputs); err != nil {
		return op, err
	}
	if err := json.Unmarshal(allocs, &op.Allocations); err != nil {
		return op, err
	}
	if err := json.Unmarshal(losses, &op.Losses); err != nil {
		return op, err
	}
	_ = json.Unmarshal(raw, &op.RawPayload)
	return op, nil
}

const operationColumns = `
	operation_key, version, supersedes_key, kind, station, occurred_at, received_at,
	source_event_id, idempotency_key, unit, container_id, handover_event_id,
	inputs, outputs, allocations, losses, raw_payload, note`

// ListOperationVersions returns every version of every operation key.
func (s *PedigreeStore) ListOperationVersions(ctx context.Context) ([]domain.OperationRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+operationColumns+`
		FROM pedigree_operations ORDER BY operation_key, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OperationRecord
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// LatestOperationVersion returns the newest version number for a key.
func (s *PedigreeStore) LatestOperationVersion(ctx context.Context, key string) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version),0) FROM pedigree_operations WHERE operation_key=$1`, key).Scan(&v)
	return v, err
}

// ---- Calculations ----------------------------------------------------------

// InsertCalc appends a calculation record.
func (s *PedigreeStore) InsertCalc(ctx context.Context, c domain.CalculationRecord) error {
	basis, _ := marshalJSON(c.Basis)
	missing, _ := marshalJSON(c.MissingBasis)
	conflict, _ := marshalJSON(c.ConflictingOps)
	witness, _ := marshalJSON(c.Witness)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pedigree_calcs
		  (calc_id, status, source_instance_id, target_instance_id, unit, lower_qty, upper_qty,
		   range_status, basis, basis_hash, missing_basis, conflicting_ops, witness,
		   resource_limit, superseded_at, superseded_by_calc, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (calc_id) DO NOTHING`,
		c.CalcID, c.Status, c.SourceInstanceID, c.TargetInstanceID, nullable(c.Unit),
		c.Lower, c.Upper, c.RangeStatus, basis, c.BasisHash, missing, conflict, witness,
		c.ResourceLimit, nullableTime(c.SupersededAt), nullableUUID(c.SupersededBy), c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert pedigree calc: %w", err)
	}
	return nil
}

// ListCalcs returns calculation records, optionally filtered by status. The
// effective status is derived from the append-only supersession table rather
// than stored mutably on the calc row.
func (s *PedigreeStore) ListCalcs(ctx context.Context, status string) ([]domain.CalculationRecord, error) {
	q := `SELECT c.calc_id,
	        CASE WHEN ss.calc_id IS NOT NULL THEN 'superseded' ELSE c.status END AS eff_status,
	        c.source_instance_id, c.target_instance_id, COALESCE(c.unit,''),
	        c.lower_qty, c.upper_qty, c.range_status, c.basis, c.basis_hash,
	        c.missing_basis, c.conflicting_ops, c.witness, c.resource_limit,
	        ss.created_at, COALESCE(ss.superseded_by_calc::text,''), c.created_at
	      FROM pedigree_calcs c
	      LEFT JOIN pedigree_calc_supersessions ss ON ss.calc_id = c.calc_id`
	args := []any{}
	if status != "" {
		q += ` WHERE CASE WHEN ss.calc_id IS NOT NULL THEN 'superseded' ELSE c.status END = $1
		       ORDER BY c.created_at DESC`
		args = append(args, status)
	} else {
		q += ` ORDER BY c.created_at DESC`
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CalculationRecord
	for rows.Next() {
		var c domain.CalculationRecord
		var basis, missing, conflict, witness []byte
		var unit string
		var supersededAt *time.Time
		if err := rows.Scan(&c.CalcID, &c.Status, &c.SourceInstanceID, &c.TargetInstanceID, &unit,
			&c.Lower, &c.Upper, &c.RangeStatus, &basis, &c.BasisHash, &missing, &conflict,
			&witness, &c.ResourceLimit, &supersededAt, &c.SupersededBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Unit = unit
		c.SupersededAt = supersededAt
		_ = json.Unmarshal(basis, &c.Basis)
		_ = json.Unmarshal(missing, &c.MissingBasis)
		_ = json.Unmarshal(conflict, &c.ConflictingOps)
		_ = json.Unmarshal(witness, &c.Witness)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCalcSuperseded appends a supersession record rather than mutating the
// append-only calc row. Idempotent: superseding the same calc twice is a no-op.
func (s *PedigreeStore) MarkCalcSuperseded(ctx context.Context, calcID, supersededBy string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pedigree_calc_supersessions (calc_id, superseded_by_calc, reason, created_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (calc_id) DO NOTHING`,
		calcID, supersededBy, "dependency invalidated by operation revision", at)
	return err
}

// SnapshotCalcRefs maps each snapshot id to the pedigree calc id it
// references. Publications pin a calculation version so a later operation
// revision cannot silently change the basis a snapshot was published on.
func (s *PedigreeStore) SnapshotCalcRefs(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, COALESCE(pedigree_calc_id::text,'')
		FROM snapshots
		WHERE pedigree_calc_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var snapID, calcID string
		if err := rows.Scan(&snapID, &calcID); err != nil {
			return nil, err
		}
		if calcID != "" {
			out[snapID] = calcID
		}
	}
	return out, rows.Err()
}

// LinkSnapshotCalc records the calculation version a publication references.
func (s *PedigreeStore) LinkSnapshotCalc(ctx context.Context, snapshotID, calcID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE snapshots SET pedigree_calc_id=$2 WHERE id=$1`, snapshotID, calcID)
	return err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableTime(t *time.Time) *time.Time { return t }

func nullableUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
