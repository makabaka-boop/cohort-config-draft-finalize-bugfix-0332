-- Configuration release service schema.
-- Statements are split on lines that contain exactly "-- @statement".

-- @statement
CREATE TABLE IF NOT EXISTS packages (
    id          BIGSERIAL PRIMARY KEY,
    checksum    TEXT NOT NULL UNIQUE,
    routes      JSONB NOT NULL,
    limits      JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- @statement
-- Packages are immutable once created: reject all row-level mutations.
CREATE OR REPLACE FUNCTION packages_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'packages are immutable and cannot be %', TG_OP;
END;
$$ LANGUAGE plpgsql;

-- @statement
DROP TRIGGER IF EXISTS packages_no_update ON packages;
CREATE TRIGGER packages_no_update BEFORE UPDATE ON packages
    FOR EACH ROW EXECUTE FUNCTION packages_reject_mutation();

-- @statement
DROP TRIGGER IF EXISTS packages_no_delete ON packages;
CREATE TRIGGER packages_no_delete BEFORE DELETE ON packages
    FOR EACH ROW EXECUTE FUNCTION packages_reject_mutation();

-- @statement
CREATE TABLE IF NOT EXISTS drafts (
    id          BIGSERIAL PRIMARY KEY,
    status      TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'finalized')),
    routes      JSONB,
    limits      JSONB,
    checksum    TEXT,
    package_id  BIGINT REFERENCES packages(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- @statement
-- Finalized drafts never change; their package_id must be populated.
ALTER TABLE drafts DROP CONSTRAINT IF EXISTS drafts_finalized_complete;
ALTER TABLE drafts ADD CONSTRAINT drafts_finalized_complete CHECK (
    status = 'open'
    OR (package_id IS NOT NULL AND checksum IS NOT NULL
        AND routes IS NOT NULL AND limits IS NOT NULL)
);

-- @statement
CREATE TABLE IF NOT EXISTS releases (
    generation         BIGSERIAL PRIMARY KEY,
    package_id         BIGINT NOT NULL REFERENCES packages(id),
    rollout_pct        INTEGER NOT NULL CHECK (rollout_pct BETWEEN 0 AND 100),
    min_client_version TEXT NOT NULL,
    action             TEXT NOT NULL CHECK (action IN ('publish', 'adjust', 'rollback')),
    supersedes         BIGINT REFERENCES releases(generation),
    note               TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- @statement
-- Single-row generation pointer. Locking this row with SELECT ... FOR UPDATE
-- serializes commits: two API instances submitting the same expected
-- generation cannot both succeed.
CREATE TABLE IF NOT EXISTS release_state (
    singleton        INTEGER PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
    head_generation  BIGINT NOT NULL DEFAULT 0
);

-- @statement
INSERT INTO release_state (singleton, head_generation)
VALUES (1, 0)
ON CONFLICT (singleton) DO NOTHING;
