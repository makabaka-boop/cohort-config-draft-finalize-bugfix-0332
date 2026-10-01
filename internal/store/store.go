// Package store implements PostgreSQL persistence for drafts, immutable
// packages and the release history.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"configrel/internal/configmodel"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrUnprocessable  = errors.New("unprocessable content")
	ErrInvalidVersion = errors.New("invalid version")
)

// Store is a pgx pool-backed repository. It is safe for concurrent use;
// API instances share no in-process state.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to the database, waits for it to accept queries and applies
// the idempotent schema migration.
func New(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{pool: pool}

	// Wait for the database to be ready (compose starts api while db is
	// still starting up).
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		if err := pool.Ping(ctx); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("database not ready: %w", lastErr)
	}

	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

//go:embed schema.sql
var schemaSQL string

func (s *Store) migrate(ctx context.Context) error {
	// Split on the "-- @statement" marker so multi-statement PL/pgSQL
	// bodies (which themselves contain semicolons) survive.
	var stmts []string
	var cur strings.Builder
	for _, line := range strings.Split(schemaSQL, "\n") {
		if strings.TrimSpace(line) == "-- @statement" {
			if cur.Len() > 0 {
				stmts = append(stmts, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if cur.Len() > 0 {
		stmts = append(stmts, cur.String())
	}

	// Concurrent API replicas starting together would race on
	// "CREATE TABLE IF NOT EXISTS" (it does not survive a concurrent
	// CREATE). Serialize startup migrations on a session advisory lock;
	// DDL itself is idempotent, so lock-holders just re-check.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	for _, stmt := range stmts {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration failed: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// migrationLockKey is an arbitrary constant identifying this service's
// migration lock.
const migrationLockKey int64 = 0x5265_6c65_6173_6531 // "Release1" truncated

// HeadGeneration returns the current publication generation (0 = none).
func (s *Store) HeadGeneration(ctx context.Context) (int64, error) {
	var gen int64
	err := s.pool.QueryRow(ctx,
		`SELECT head_generation FROM release_state WHERE singleton = 1`).Scan(&gen)
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// CreateDraft inserts an empty open draft.
func (s *Store) CreateDraft(ctx context.Context) (*configmodel.Draft, error) {
	return s.scanDraft(s.pool.QueryRow(ctx,
		`INSERT INTO drafts DEFAULT VALUES
		 RETURNING id, status, routes, limits, checksum, package_id, created_at, updated_at`))
}

// GetDraft fetches a draft by id.
func (s *Store) GetDraft(ctx context.Context, id int64) (*configmodel.Draft, error) {
	return s.scanDraft(s.pool.QueryRow(ctx,
		`SELECT id, status, routes, limits, checksum, package_id, created_at, updated_at
		 FROM drafts WHERE id = $1`, id))
}

// UpdateDraft overwrites the two groups of an open draft with canonical JSON.
func (s *Store) UpdateDraft(ctx context.Context, id int64, routes, limits []byte) (*configmodel.Draft, error) {
	return s.scanDraft(s.pool.QueryRow(ctx,
		`UPDATE drafts
		    SET routes = $2, limits = $3, updated_at = now()
		  WHERE id = $1 AND status = 'open'
		 RETURNING id, status, routes, limits, checksum, package_id, created_at, updated_at`,
		id, string(routes), string(limits)))
}

func (s *Store) scanDraft(row pgx.Row) (*configmodel.Draft, error) {
	var d configmodel.Draft
	var routes, limits, checksum, status *string
	if err := row.Scan(&d.ID, &status, &routes, &limits, &checksum, &d.PackageID, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.Status = deref(status)
	if routes != nil {
		d.Routes = json.RawMessage(*routes)
	}
	if limits != nil {
		d.Limits = json.RawMessage(*limits)
	}
	d.Checksum = deref(checksum)
	return &d, nil
}

// FinalizeResult reports what finalization produced.
type FinalizeResult struct {
	Draft   *configmodel.Draft
	Package *configmodel.Package
	Reused  bool // true if an identical package already existed
}

// FinalizeDraft validates cross-group references and atomically converts the
// draft into an immutable package. Identical content is content-addressed:
// the same (routes, limits) pair always resolves to the same package id.
func (s *Store) FinalizeDraft(ctx context.Context, id int64, content *configmodel.DraftContent) (*FinalizeResult, error) {
	routes, limits, err := content.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	checksum := configmodel.Checksum(routes, limits)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Re-read under row lock so two finalizations of the same draft cannot race.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM drafts WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if status != "open" {
		return nil, fmt.Errorf("%w: draft %d is already finalized", ErrConflict, id)
	}

	// Fetch the immutable package by content checksum, or insert it.
	// A plain INSERT without ON CONFLICT is required: the packages table
	// rejects UPDATE via an immutability trigger. The unique constraint
	// still guarantees one canonical row per checksum under concurrency.
	var pkg *configmodel.Package
	var reused bool
	pkg, err = s.getPackageTx(ctx, tx, -1, checksum)
	switch {
	case err == nil:
		reused = true
	case errors.Is(err, ErrNotFound):
		pkg = &configmodel.Package{}
		err = tx.QueryRow(ctx,
			`INSERT INTO packages (checksum, routes, limits)
			 VALUES ($1, $2, $3)
			 RETURNING id, checksum, routes, limits, created_at`,
			checksum, string(routes), string(limits)).
			Scan(&pkg.ID, &pkg.Checksum, &pkg.Routes, &pkg.Limits, &pkg.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				// Raced with another finalizer: reuse its identical package.
				pkg, err = s.getPackageTx(ctx, tx, -1, checksum)
				if err != nil {
					return nil, err
				}
				reused = true
			} else {
				return nil, err
			}
		}
	default:
		return nil, err
	}

	d, err := s.scanDraft(tx.QueryRow(ctx,
		`UPDATE drafts
		    SET status = 'finalized', routes = $2, limits = $3,
		        checksum = $4, package_id = $5, updated_at = now()
		  WHERE id = $1
		 RETURNING id, status, routes, limits, checksum, package_id, created_at, updated_at`,
		id, string(routes), string(limits), checksum, pkg.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &FinalizeResult{Draft: d, Package: pkg, Reused: reused}, nil
}

// getPackageTx fetches a package row within a tx by id (id >= 0) or checksum.
func (s *Store) getPackageTx(ctx context.Context, tx pgx.Tx, id int64, checksum string) (*configmodel.Package, error) {
	var row pgx.Row
	if id >= 0 {
		row = tx.QueryRow(ctx,
			`SELECT id, checksum, routes, limits, created_at FROM packages WHERE id = $1`, id)
	} else {
		row = tx.QueryRow(ctx,
			`SELECT id, checksum, routes, limits, created_at FROM packages WHERE checksum = $1`, checksum)
	}
	var p configmodel.Package
	if err := row.Scan(&p.ID, &p.Checksum, &p.Routes, &p.Limits, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// GetPackage fetches a complete package snapshot.
func (s *Store) GetPackage(ctx context.Context, id int64) (*configmodel.Package, error) {
	var p configmodel.Package
	err := s.pool.QueryRow(ctx,
		`SELECT id, checksum, routes, limits, created_at FROM packages WHERE id = $1`, id).
		Scan(&p.ID, &p.Checksum, &p.Routes, &p.Limits, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// commitAction selects which history verb a commit records.
type commitAction = string

const (
	ActionPublish  commitAction = "publish"
	ActionAdjust   commitAction = "adjust"
	ActionRollback commitAction = "rollback"
)

// Commit is a request to create a new release generation. Exactly one of
// these succeeds for a given ExpectedGeneration: the pointer row is locked
// for the duration of the transaction.
type Commit struct {
	ExpectedGeneration int64
	PackageID          int64
	RolloutPct         int
	MinClientVersion   string
	Action             commitAction
	Note               string
}

// CommitResult returns the new release and the previous head (may be nil).
type CommitResult struct {
	Release *configmodel.Release
	Prev    *configmodel.Release
}

// CommitRelease appends a new release generation after verifying the
// expected generation. Concurrent commits serialize on release_state; the
// loser sees ErrConflict and its insert never happens.
func (s *Store) CommitRelease(ctx context.Context, c Commit) (*CommitResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var head int64
	err = tx.QueryRow(ctx,
		`SELECT head_generation FROM release_state WHERE singleton = 1 FOR UPDATE`).
		Scan(&head)
	if err != nil {
		return nil, err
	}
	if head != c.ExpectedGeneration {
		return nil, fmt.Errorf("%w: expected generation %d but head is %d",
			ErrConflict, c.ExpectedGeneration, head)
	}

	// A publish must reference a package that actually exists. Adjust
	// keeps the current package, rollback references a historical one;
	// both package ids are derived from rows the caller already read,
	// but the FK check below enforces completeness in all cases.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM packages WHERE id = $1`, c.PackageID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: package %d does not exist", ErrNotFound, c.PackageID)
		}
		return nil, err
	}

	r, err := scanRelease(tx.QueryRow(ctx,
		`INSERT INTO releases
		    (package_id, rollout_pct, min_client_version, action, supersedes, note)
		 VALUES ($1, $2, $3, $4, CASE WHEN $5 = 0 THEN NULL ELSE $5 END, $6)
		 RETURNING generation, package_id, rollout_pct, min_client_version,
		           action, note, created_at`,
		c.PackageID, c.RolloutPct, c.MinClientVersion, string(c.Action), head, c.Note))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE release_state SET head_generation = $1 WHERE singleton = 1`,
		r.Generation); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	result := &CommitResult{Release: r}
	if head > 0 {
		prev, err := s.getRelease(ctx, head)
		if err != nil {
			return nil, err
		}
		result.Prev = prev
	}
	return result, nil
}

// LastReleaseForPackage returns the newest release that pointed at pkg.
func (s *Store) LastReleaseForPackage(ctx context.Context, pkg int64) (*configmodel.Release, error) {
	return scanRelease(s.pool.QueryRow(ctx,
		`SELECT generation, package_id, rollout_pct, min_client_version,
		        action, note, created_at
		 FROM releases WHERE package_id = $1
		 ORDER BY generation DESC LIMIT 1`, pkg))
}

// GetRelease returns a single release row without embedded package.
func (s *Store) GetRelease(ctx context.Context, generation int64) (*configmodel.Release, error) {
	return s.getRelease(ctx, generation)
}

func (s *Store) getRelease(ctx context.Context, generation int64) (*configmodel.Release, error) {
	return scanRelease(s.pool.QueryRow(ctx,
		`SELECT generation, package_id, rollout_pct, min_client_version,
		        action, note, created_at
		 FROM releases WHERE generation = $1`, generation))
}

// ReleaseWithPackage is one history entry plus its complete snapshot.
func (s *Store) GetReleaseWithPackage(ctx context.Context, generation int64) (*configmodel.Release, error) {
	r, err := s.scanReleasePackage(s.pool.QueryRow(ctx, releaseSelectPackage+`
		 WHERE r.generation = $1`, generation))
	if err != nil {
		return nil, err
	}
	return r, nil
}

const releaseSelectPackage = `
	SELECT r.generation, r.package_id, r.rollout_pct, r.min_client_version,
	       r.action, r.note, r.created_at,
	       p.id, p.checksum, p.routes, p.limits, p.created_at
	  FROM releases r
	  JOIN packages p ON p.id = r.package_id`

// ListReleases returns the full publication history newest-first, each with
// its complete package snapshot. The query is a single read-consistent
// snapshot, so history can never mix generations with mismatched packages.
func (s *Store) ListReleases(ctx context.Context) ([]*configmodel.Release, error) {
	rows, err := s.pool.Query(ctx, releaseSelectPackage+`
		ORDER BY r.generation DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*configmodel.Release
	for rows.Next() {
		r, err := s.scanReleasePackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListReleaseSnapshots returns every release with its embedded package,
// newest first, as one read-consistent snapshot (used by resolution).
func (s *Store) ListReleaseSnapshots(ctx context.Context) ([]configmodel.Snapshot, error) {
	rows, err := s.pool.Query(ctx, releaseSelectPackage+`
		ORDER BY r.generation DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []configmodel.Snapshot
	for rows.Next() {
		r, err := s.scanReleasePackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, configmodel.Snapshot{Release: r, Package: r.Package})
	}
	return out, rows.Err()
}

func scanRelease(row pgx.Row) (*configmodel.Release, error) {
	var r configmodel.Release
	err := row.Scan(&r.Generation, &r.PackageID, &r.RolloutPct,
		&r.MinClientVersion, &r.Action, &r.Note, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

func (s *Store) scanReleasePackage(row pgx.Row) (*configmodel.Release, error) {
	var r configmodel.Release
	var p configmodel.Package
	err := row.Scan(&r.Generation, &r.PackageID, &r.RolloutPct,
		&r.MinClientVersion, &r.Action, &r.Note, &r.CreatedAt,
		&p.ID, &p.Checksum, &p.Routes, &p.Limits, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Package = &p
	return &r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
