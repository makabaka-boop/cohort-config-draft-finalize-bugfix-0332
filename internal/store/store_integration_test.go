package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"configrel/internal/configmodel"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return testStoreSchema(t, "it_"+sanitize(t.Name()))
}

func testStoreSchema(t *testing.T, schema string) *Store {
	t.Helper()
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	if _, err := openExec(ctx, rawURL, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = openExec(ctx, rawURL, "DROP SCHEMA "+schema+" CASCADE")
	})
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := New(ctx, u.String())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func sanitize(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b = append(b, byte(r))
		} else {
			b = append(b, '_')
		}
	}
	return string(b)
}

func openExec(ctx context.Context, url, sql string) (string, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	tag, err := pool.Exec(ctx, sql)
	return tag.String(), err
}

func validContent(path string) *configmodel.DraftContent {
	return &configmodel.DraftContent{
		Routes: []configmodel.Route{
			{ID: "r1", Path: path, Upstream: "svc", LimitID: "l1"},
		},
		Limits: []configmodel.Limit{
			{ID: "l1", Name: "basic", RPM: 100},
		},
	}
}

func TestFinalizeIsImmutableAndContentAddressed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	d, err := st.CreateDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	putContent(t, st, ctx, d.ID, validContent("/a"))
	res, err := st.FinalizeDraft(ctx, d.ID, validContent("/a"))
	if err != nil {
		t.Fatal(err)
	}
	pkg1 := res.Package.ID

	// Re-finalizing the same draft conflicts (drafts become immutable too).
	if _, err := st.FinalizeDraft(ctx, d.ID, validContent("/a")); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict re-finalizing draft, got %v", err)
	}

	// Identical content from a new draft reuses the package row.
	d2, _ := st.CreateDraft(ctx)
	putContent(t, st, ctx, d2.ID, validContent("/a"))
	res2, err := st.FinalizeDraft(ctx, d2.ID, validContent("/a"))
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Reused || res2.Package.ID != pkg1 {
		t.Fatalf("identical content must reuse package %d, got %d reused=%v",
			pkg1, res2.Package.ID, res2.Reused)
	}

	// Changed routes produce a different package.
	d3, _ := st.CreateDraft(ctx)
	c := validContent("/b")
	r, l, _ := c.CanonicalJSON()
	if _, err := st.UpdateDraft(ctx, d3.ID, r, l); err != nil {
		t.Fatal(err)
	}
	res3, err := st.FinalizeDraft(ctx, d3.ID, c)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Package.ID == pkg1 {
		t.Fatal("changed routes must produce a new package")
	}

	// Packages cannot be updated or deleted.
	schemaURL := withSchema(os.Getenv("TEST_DATABASE_URL"), currentSchema(st, t))
	if _, err := openExec(ctx, schemaURL,
		fmt.Sprintf("UPDATE packages SET checksum = 'tampered' WHERE id = %d", pkg1)); err == nil {
		t.Fatal("expected UPDATE on packages to be rejected by immutability trigger")
	}
}

func TestConcurrentCommitsExactlyOneWins(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	d, _ := st.CreateDraft(ctx)
	putContent(t, st, ctx, d.ID, validContent("/p"))
	res, err := st.FinalizeDraft(ctx, d.ID, validContent("/p"))
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	winners, losers := 0, 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.CommitRelease(ctx, Commit{
				ExpectedGeneration: 0,
				PackageID:          res.Package.ID,
				RolloutPct:         100,
				MinClientVersion:   "1.0",
				Action:             ActionPublish,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else if errors.Is(err, ErrConflict) {
				losers++
			}
		}()
	}
	wg.Wait()
	if winners != 1 || losers != n-1 {
		t.Fatalf("expected exactly 1 winner and %d losers, got winners=%d losers=%d", n-1, winners, losers)
	}

	head, err := st.HeadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != 1 {
		t.Fatalf("expected head generation 1, got %d", head)
	}

	// Stale expected generation loses to the new head.
	_, err = st.CommitRelease(ctx, Commit{
		ExpectedGeneration: 0, PackageID: res.Package.ID,
		RolloutPct: 50, MinClientVersion: "1.0", Action: ActionAdjust,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict on stale generation, got %v", err)
	}
}

func TestAdjustAndRollbackCreateGenerations(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	mkPkg := func(path string) int64 {
		d, _ := st.CreateDraft(ctx)
		c := validContent(path)
		putContent(t, st, ctx, d.ID, c)
		res, err := st.FinalizeDraft(ctx, d.ID, c)
		if err != nil {
			t.Fatal(err)
		}
		return res.Package.ID
	}
	pkgA := mkPkg("/a")
	pkgB := mkPkg("/b")

	commit := func(expect int64, pkg int64, pct int, minV, action string) int64 {
		t.Helper()
		res, err := st.CommitRelease(ctx, Commit{
			ExpectedGeneration: expect, PackageID: pkg,
			RolloutPct: pct, MinClientVersion: minV, Action: action,
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		return res.Release.Generation
	}

	g1 := commit(0, pkgA, 10, "1.0", ActionPublish)
	g2 := commit(g1, pkgA, 50, "1.0", ActionAdjust) // adjust = new generation
	if g2 != g1+1 {
		t.Fatalf("adjust must create a new generation")
	}
	g3 := commit(g2, pkgB, 100, "2.0", ActionPublish)
	g4 := commit(g3, pkgA, 100, "1.0", ActionRollback) // rollback = new generation
	if g4 != g3+1 {
		t.Fatalf("rollback must create a new generation")
	}

	releases, err := st.ListReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 4 {
		t.Fatalf("expected 4 releases, got %d", len(releases))
	}
	head, _ := st.HeadGeneration(ctx)
	if head != g4 {
		t.Fatalf("head = %d, want %d", head, g4)
	}

	// History entries always embed their complete package snapshot.
	headRel, err := st.GetReleaseWithPackage(ctx, g4)
	if err != nil {
		t.Fatal(err)
	}
	if headRel.Package.ID != pkgA {
		t.Fatalf("rollback head must point at complete package %d, got %d", pkgA, headRel.Package.ID)
	}

	// Rollout boundaries are enforced by the database.
	_, err = st.CommitRelease(ctx, Commit{
		ExpectedGeneration: g4, PackageID: pkgA,
		RolloutPct: 101, MinClientVersion: "1.0", Action: ActionAdjust,
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected database check violation for pct=101, got %v", err)
	}
}

func currentSchema(st *Store, t *testing.T) string {
	t.Helper()
	var name string
	if err := st.pool.QueryRow(context.Background(), "SELECT current_schema()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

func withSchema(rawURL, schema string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func mustCanon(t *testing.T, c *configmodel.DraftContent) ([]byte, []byte) {
	t.Helper()
	r, l, err := c.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return r, l
}

func putContent(t *testing.T, st *Store, ctx context.Context, id int64, c *configmodel.DraftContent) {
	t.Helper()
	r, l := mustCanon(t, c)
	if _, err := st.UpdateDraft(ctx, id, r, l); err != nil {
		t.Fatal(err)
	}
}
