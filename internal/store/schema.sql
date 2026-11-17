-- Aviation cargo security event data-quality & status platform.
-- All source-of-truth tables are append-only; derived projections are rebuilt.

CREATE TABLE IF NOT EXISTS contracts (
    id              bigserial PRIMARY KEY,
    source          text        NOT NULL,
    message_type    text        NOT NULL,
    version         integer     NOT NULL,
    compat          text        NOT NULL CHECK (compat IN ('strict','backward','free')),
    definition      jsonb       NOT NULL,
    status          text        NOT NULL DEFAULT 'active',
    registered_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, message_type, version)
);

CREATE TABLE IF NOT EXISTS raw_events (
    seq                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id            uuid        NOT NULL UNIQUE,
    source              text        NOT NULL,
    message_type        text        NOT NULL,
    source_code         text,
    contract_version    integer     NOT NULL,
    idempotency_key     text        NOT NULL,
    dedupe_key          text        NOT NULL,
    business_keys       jsonb       NOT NULL,
    raw_business_keys   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    package_id          text,
    package_key         text,
    package_revision    text,
    shipment_key        text,
    device_id           text,
    device_event_no     text,
    evidence_indexes    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    occurred_at         timestamptz NOT NULL,
    received_at         timestamptz NOT NULL,
    ingested_at         timestamptz NOT NULL DEFAULT now(),
    payload             jsonb       NOT NULL,
    content_hash        text        NOT NULL,
    supersedes_event_id uuid,
    missing_fields      jsonb       NOT NULL DEFAULT '[]'::jsonb,
    unmapped_fields     jsonb       NOT NULL DEFAULT '[]'::jsonb,
    UNIQUE (source, dedupe_key)
);

CREATE INDEX IF NOT EXISTS raw_events_received_idx ON raw_events (received_at, seq);
CREATE INDEX IF NOT EXISTS raw_events_package_idx  ON raw_events (package_key);
CREATE INDEX IF NOT EXISTS raw_events_shipment_idx ON raw_events (shipment_key);
CREATE INDEX IF NOT EXISTS raw_events_type_idx     ON raw_events (message_type);
CREATE INDEX IF NOT EXISTS raw_events_device_idx   ON raw_events (device_id);
CREATE INDEX IF NOT EXISTS raw_events_hashes_idx   ON raw_events (source, content_hash);

CREATE TABLE IF NOT EXISTS intake_attempts (
    id              bigserial PRIMARY KEY,
    source          text        NOT NULL,
    message_type    text        NOT NULL,
    source_code     text,
    idempotency_key text        NOT NULL,
    content_hash    text,
    occurred_at     timestamptz,
    received_at     timestamptz NOT NULL,
    outcome         text        NOT NULL CHECK (outcome IN
                        ('accepted','duplicate_identical','duplicate_conflicting','rejected')),
    detail          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    raw_event_id    uuid,
    attempted_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS intake_attempts_key_idx ON intake_attempts (source, idempotency_key);
CREATE INDEX IF NOT EXISTS intake_attempts_time_idx ON intake_attempts (received_at);

-- Identity graph. Links are never mutated; a link is closed by appending a
-- retraction, and splits/merges/container changes append new links. Every row
-- carries the knowledge time (received_at of the causing event) so snapshots
-- can be replayed against exactly the knowledge available at their watermark.
CREATE TABLE IF NOT EXISTS graph_links (
    id              bigserial PRIMARY KEY,
    link_key        text        NOT NULL,
    link_version    integer     NOT NULL,
    kind            text        NOT NULL,
    subject_type    text        NOT NULL,
    subject_key     text        NOT NULL,
    object_type     text        NOT NULL,
    object_key      text        NOT NULL,
    valid_from      timestamptz NOT NULL,
    knowledge_at    timestamptz NOT NULL,
    package_revision text,
    source_event_id uuid        NOT NULL,
    attributes      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (link_key, link_version)
);

CREATE INDEX IF NOT EXISTS graph_links_subject_idx ON graph_links (subject_type, subject_key);
CREATE INDEX IF NOT EXISTS graph_links_object_idx  ON graph_links (object_type, object_key);
CREATE INDEX IF NOT EXISTS graph_links_kind_idx    ON graph_links (kind);

CREATE TABLE IF NOT EXISTS graph_retractions (
    id              bigserial PRIMARY KEY,
    link_id         bigint      NOT NULL REFERENCES graph_links(id),
    retracted_at    timestamptz NOT NULL,
    knowledge_at    timestamptz NOT NULL,
    reason          text        NOT NULL,
    source_event_id uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS graph_retractions_link_idx ON graph_retractions (link_id);

-- Disposition of each event against the identity graph: was the package still
-- active and at the referenced revision when the event arrived? Recomputed at
-- publish and appended idempotently.
CREATE TABLE IF NOT EXISTS evidence_dispositions (
    id              bigserial PRIMARY KEY,
    event_id        uuid        NOT NULL,
    package_key     text        NOT NULL DEFAULT '',
    shipment_key    text,
    disposition     text        NOT NULL,
    reason          text        NOT NULL,
    knowledge_at    timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, disposition, package_key)
);

CREATE INDEX IF NOT EXISTS evidence_dispositions_shipment_idx
    ON evidence_dispositions (shipment_key);

CREATE TABLE IF NOT EXISTS watermarks (
    id                      uuid        PRIMARY KEY,
    scope_type              text        NOT NULL,
    scope_key               text        NOT NULL,
    cutoff_received_at      timestamptz NOT NULL,
    required_sources        jsonb       NOT NULL,
    require_handover        boolean     NOT NULL DEFAULT true,
    mode                    text        NOT NULL CHECK (mode IN ('wait_complete','early_provisional')),
    label                   text,
    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS watermark_closures (
    id              bigserial PRIMARY KEY,
    watermark_id    uuid        NOT NULL REFERENCES watermarks(id),
    source          text        NOT NULL,
    completed_at    timestamptz NOT NULL,
    note            text,
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (watermark_id, source)
);

CREATE TABLE IF NOT EXISTS rule_violations (
    id                bigserial PRIMARY KEY,
    code              text        NOT NULL,
    severity          text        NOT NULL CHECK (severity IN ('info','warning','error','blocker')),
    subject_type      text        NOT NULL,
    subject_key       text        NOT NULL,
    dedupe_key        text        NOT NULL,
    knowledge_at      timestamptz NOT NULL,
    detail            jsonb       NOT NULL,
    related_event_ids jsonb       NOT NULL DEFAULT '[]'::jsonb,
    first_detected_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (code, subject_type, subject_key, dedupe_key)
);

CREATE INDEX IF NOT EXISTS rule_violations_subject_idx ON rule_violations (subject_type, subject_key);

CREATE TABLE IF NOT EXISTS status_assessments (
    id                  uuid        PRIMARY KEY,
    scope_type          text        NOT NULL,
    scope_key           text        NOT NULL,
    cutoff_received_at  timestamptz NOT NULL,
    max_seq             bigint      NOT NULL,
    status              text        NOT NULL,
    security_result     text,
    evidence_completeness double precision NOT NULL,
    result              jsonb       NOT NULL,
    inputs              jsonb       NOT NULL,
    result_hash         text        NOT NULL,
    computed_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS status_assessments_scope_idx ON status_assessments (scope_type, scope_key);

CREATE TABLE IF NOT EXISTS snapshots (
    id                    uuid        PRIMARY KEY,
    scope_type            text        NOT NULL,
    scope_key             text        NOT NULL,
    version               integer     NOT NULL,
    snapshot_kind         text        NOT NULL CHECK (snapshot_kind IN ('provisional','formal')),
    watermark_id          uuid        NOT NULL REFERENCES watermarks(id),
    assessment_id         uuid        NOT NULL REFERENCES status_assessments(id),
    cutoff_received_at    timestamptz NOT NULL,
    status                text        NOT NULL,
    security_result       text,
    evidence_completeness double precision NOT NULL,
    consumable_confirmed  boolean     NOT NULL,
    payload               jsonb       NOT NULL,
    quality_summary       jsonb       NOT NULL,
    diff                  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    pedigree_calc_id      uuid,
    supersedes_snapshot_id uuid,
    published_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_type, scope_key, version)
);

CREATE INDEX IF NOT EXISTS snapshots_scope_idx ON snapshots (scope_type, scope_key, version);
CREATE INDEX IF NOT EXISTS snapshots_published_idx ON snapshots (published_at);

-- ===========================================================================
-- Packaging pedigree (来源追溯 / 数量核对 / 计算版本)
-- All tables are append-only. Operation corrections add a NEW version row;
-- calculations pin the operation versions they used and are never overwritten.
-- ===========================================================================

-- Registered package instances. A label alone is not an identity: instances
-- are co-located by issuer + scope + active interval.
CREATE TABLE IF NOT EXISTS pedigree_instances (
    id              BIGSERIAL PRIMARY KEY,
    instance_id     TEXT NOT NULL,
    label           TEXT NOT NULL,
    issuer          TEXT NOT NULL,
    scope           TEXT NOT NULL,
    revision        TEXT,
    role            TEXT NOT NULL DEFAULT 'package',
    active_from     TIMESTAMPTZ NOT NULL,
    active_to       TIMESTAMPTZ,
    source_event_id TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (instance_id)
);
CREATE INDEX IF NOT EXISTS pedigree_instances_label_scope_idx
    ON pedigree_instances (scope, label);

-- Evidence-backed relabel aliases (换标签别名).
CREATE TABLE IF NOT EXISTS pedigree_aliases (
    id              BIGSERIAL PRIMARY KEY,
    alias_id        TEXT NOT NULL UNIQUE,
    from_instance_id TEXT NOT NULL,
    to_instance_id   TEXT NOT NULL,
    effective_at    TIMESTAMPTZ NOT NULL,
    basis_event_id  TEXT NOT NULL,
    source_event_id TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pedigree_aliases_from_idx ON pedigree_aliases (from_instance_id);

-- Interval-dated containment / stow relations (装载区间).
CREATE TABLE IF NOT EXISTS pedigree_stows (
    id               BIGSERIAL PRIMARY KEY,
    stow_id          TEXT NOT NULL UNIQUE,
    child_instance_id TEXT NOT NULL,
    parent_kind      TEXT NOT NULL,
    parent_ref       TEXT NOT NULL,
    valid_from       TIMESTAMPTZ NOT NULL,
    valid_to         TIMESTAMPTZ,
    handover_event_id TEXT,
    source_event_id  TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pedigree_stows_child_idx ON pedigree_stows (child_instance_id);
CREATE INDEX IF NOT EXISTS pedigree_stows_parent_idx ON pedigree_stows (parent_ref);

-- Packaging operations, versioned. A correction inserts a new (operation_key,
-- version) row with supersedes pointing at the prior version's event.
CREATE TABLE IF NOT EXISTS pedigree_operations (
    id                BIGSERIAL PRIMARY KEY,
    operation_key     TEXT NOT NULL,
    version           INTEGER NOT NULL,
    supersedes_key    TEXT,
    kind              TEXT NOT NULL, -- relabel|intact_stow|split|mixed_repack
    station           TEXT NOT NULL,
    occurred_at       TIMESTAMPTZ NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL,
    source_event_id   TEXT NOT NULL,
    idempotency_key   TEXT NOT NULL,
    unit              TEXT,
    container_id      TEXT,
    handover_event_id TEXT,
    inputs            JSONB NOT NULL DEFAULT '[]'::jsonb,
    outputs           JSONB NOT NULL DEFAULT '[]'::jsonb,
    allocations       JSONB NOT NULL DEFAULT '[]'::jsonb,
    losses            JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw_payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    note              TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (operation_key, version)
);
CREATE INDEX IF NOT EXISTS pedigree_operations_kind_time_idx
    ON pedigree_operations (kind, occurred_at);
CREATE INDEX IF NOT EXISTS pedigree_operations_idem_idx
    ON pedigree_operations (idempotency_key);

-- On-demand source-range calculations with their pinned basis. Downstream
-- snapshots reference calc_id; a revision supersedes rather than mutates.
CREATE TABLE IF NOT EXISTS pedigree_calcs (
    id                  BIGSERIAL PRIMARY KEY,
    calc_id             UUID NOT NULL UNIQUE,
    status              TEXT NOT NULL, -- completed|unverified|superseded
    source_instance_id  TEXT NOT NULL,
    target_instance_id  TEXT NOT NULL,
    unit                TEXT,
    lower_qty           BIGINT NOT NULL DEFAULT 0,
    upper_qty           BIGINT NOT NULL DEFAULT 0,
    range_status        TEXT NOT NULL,
    basis               JSONB NOT NULL DEFAULT '[]'::jsonb,
    basis_hash          TEXT NOT NULL,
    missing_basis       JSONB NOT NULL DEFAULT '[]'::jsonb,
    conflicting_ops     JSONB NOT NULL DEFAULT '[]'::jsonb,
    witness             JSONB NOT NULL DEFAULT '[]'::jsonb,
    resource_limit      BOOLEAN NOT NULL DEFAULT FALSE,
    superseded_at       TIMESTAMPTZ,
    superseded_by_calc  UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pedigree_calcs_pair_idx
    ON pedigree_calcs (source_instance_id, target_instance_id);
CREATE INDEX IF NOT EXISTS pedigree_calcs_status_idx ON pedigree_calcs (status);

-- Append-only record that a calculation version was superseded. The original
-- calc row is never mutated; current status is derived by left-joining this.
CREATE TABLE IF NOT EXISTS pedigree_calc_supersessions (
    id                  BIGSERIAL PRIMARY KEY,
    calc_id             UUID NOT NULL,
    superseded_by_calc  UUID NOT NULL,
    reason              TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (calc_id)
);
CREATE INDEX IF NOT EXISTS pedigree_calc_super_by_idx
    ON pedigree_calc_supersessions (superseded_by_calc);

-- Append-only enforcement -----------------------------------------------------
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table %.% is append-only: % is not permitted', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP;
END;
$$;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'contracts','raw_events','intake_attempts','graph_links','graph_retractions',
        'evidence_dispositions','watermarks','watermark_closures','rule_violations',
        'status_assessments','snapshots',
        'pedigree_instances','pedigree_aliases','pedigree_stows',
        'pedigree_operations','pedigree_calcs','pedigree_calc_supersessions'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_trigger
            WHERE tgname = t || '_append_only' AND tgrelid = t::regclass
        ) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION forbid_mutation()',
                t || '_append_only', t);
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM pg_trigger
            WHERE tgname = t || '_no_truncate' AND tgrelid = t::regclass
        ) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation()',
                t || '_no_truncate', t);
        END IF;
    END LOOP;
END;
$$;
