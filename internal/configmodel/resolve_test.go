package configmodel

import "testing"

func snap(gen int64, pkgID int64, pct int, minV, action string) Snapshot {
	return Snapshot{
		Release: &Release{
			Generation:       gen,
			PackageID:        pkgID,
			RolloutPct:       pct,
			MinClientVersion: minV,
			Action:           action,
		},
		Package: &Package{ID: pkgID, Checksum: "sum-" + string(rune('a'+pkgID-1))},
	}
}

// find a client id whose bucket lands inside/outside a given pct.
func clientInBucket(pct int) (in, out string) {
	for i := 0; ; i++ {
		id := "c-" + itoa(i)
		if in == "" && InCanaryBucket(id, pct) {
			in = id
		}
		if out == "" && !InCanaryBucket(id, pct) {
			out = id
		}
		if in != "" && out != "" {
			return in, out
		}
	}
}

func TestResolveOldClientFallsBackToNearestCompatible(t *testing.T) {
	snaps := []Snapshot{
		snap(3, 3, 100, "2.0", "publish"), // head requires client 2.x
		snap(2, 2, 100, "1.5", "publish"), // needs 1.5
		snap(1, 1, 100, "1.0", "publish"),
	}

	// Client 1.0 skips gen3 and gen2, lands on gen1 (nearest compatible).
	got, err := Resolve(snaps, "1.0", "any-client", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Package.ID != 1 || got.Release.Generation != 1 {
		t.Fatalf("client 1.0 should resolve package 1/generation 1, got %+v", got)
	}
	if got.Head {
		t.Fatal("fallback resolution must not report served_head")
	}

	// Client 1.9 skips only gen3.
	got, _ = Resolve(snaps, "1.9", "any-client", 3)
	if got.Package.ID != 2 || got.Release.Generation != 2 {
		t.Fatalf("client 1.9 should resolve package 2, got %+v", got)
	}

	// New client gets the head.
	got, _ = Resolve(snaps, "2.0", "any-client", 3)
	if got.Package.ID != 3 || !got.Head {
		t.Fatalf("client 2.0 should resolve head package 3, got %+v", got)
	}
}

func TestResolveCanaryStableGrouping(t *testing.T) {
	snaps := []Snapshot{
		snap(2, 2, 30, "1.0", "publish"), // partial head
		snap(1, 1, 100, "1.0", "publish"),
	}
	in, out := clientInBucket(30)

	got, _ := Resolve(snaps, "1.0", in, 2)
	if got.Package.ID != 2 || !got.Head {
		t.Fatalf("canary client should get package 2 head, got %+v", got)
	}

	got, _ = Resolve(snaps, "1.0", out, 2)
	if got.Package.ID != 1 || got.Head {
		t.Fatalf("non-canary client should fall back to package 1, got %+v", got)
	}

	// Resolution must be stable across repeated calls (simulates restart).
	for i := 0; i < 10; i++ {
		g1, _ := Resolve(snaps, "1.0", in, 2)
		g2, _ := Resolve(snaps, "1.0", out, 2)
		if g1.Package.ID != 2 || g2.Package.ID != 1 {
			t.Fatal("resolution is not stable across calls")
		}
	}
}

func TestResolveZeroPercentServesPrevious(t *testing.T) {
	snaps := []Snapshot{
		snap(2, 2, 0, "1.0", "publish"),
		snap(1, 1, 100, "1.0", "publish"),
	}
	got, _ := Resolve(snaps, "1.0", "anything", 2)
	if got.Package.ID != 1 {
		t.Fatalf("0%% rollout must serve nobody the head, got %+v", got)
	}
}

func TestResolveRollbackPointsAtCompleteSnapshot(t *testing.T) {
	snaps := []Snapshot{
		snap(3, 1, 100, "1.0", "rollback"), // head rolls package 1 back out
		snap(2, 2, 100, "2.0", "publish"),
		snap(1, 1, 100, "1.0", "publish"),
	}
	got, _ := Resolve(snaps, "1.0", "c", 3)
	if got.Release.Generation != 3 || got.Package.ID != 1 {
		t.Fatalf("rollback head should serve package 1 via generation 3, got %+v", got)
	}
	if !got.Head || got.Reason != "head" {
		t.Fatalf("rollback head should report head, got %+v", got)
	}
}

func TestResolveNoCompatibleSnapshot(t *testing.T) {
	snaps := []Snapshot{snap(1, 1, 100, "3.0", "publish")}
	got, err := Resolve(snaps, "1.0", "c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil resolution, got %+v", got)
	}
}
