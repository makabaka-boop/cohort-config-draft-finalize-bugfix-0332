package configmodel

import (
	"testing"
)

func sampleContent() *DraftContent {
	return &DraftContent{
		Routes: []Route{
			{ID: "r1", Path: "/a", Upstream: "svc-a", LimitID: "l1"},
			{ID: "r2", Path: "/b", Upstream: "svc-b", LimitID: "l2"},
		},
		Limits: []Limit{
			{ID: "l1", Name: "basic", RPM: 100, Burst: 20},
			{ID: "l2", Name: "heavy", RPM: 1000},
		},
	}
}

func TestValidateAcceptsValidDraft(t *testing.T) {
	if problems := sampleContent().Validate(); len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
}

func TestValidateCrossGroupReference(t *testing.T) {
	c := sampleContent()
	c.Routes[0].LimitID = "missing"
	problems := c.Validate()
	found := false
	for _, p := range problems {
		if p == `routes[0].limit_id "missing" has no matching limits entry` {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cross-group reference error, got %v", problems)
	}
}

func TestValidateRejectsEmptyGroupsAndDuplicates(t *testing.T) {
	c := &DraftContent{}
	if problems := c.Validate(); len(problems) != 2 {
		t.Fatalf("expected 2 problems for empty draft, got %v", problems)
	}

	c = sampleContent()
	c.Limits = append(c.Limits, Limit{ID: "l1", Name: "dup", RPM: 1})
	problems := c.Validate()
	found := false
	for _, p := range problems {
		if p == `duplicate limit id "l1"` {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected duplicate id error, got %v", problems)
	}
}

func TestChecksumBindsBothGroups(t *testing.T) {
	c := sampleContent()
	r1, l1, err := c.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	base := Checksum(r1, l1)

	// New routes with the SAME old limits must produce a different package.
	c2 := sampleContent()
	c2.Routes[0].Path = "/changed"
	r2, l2, err := c2.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if Checksum(r2, l2) == base {
		t.Fatal("changing routes must change the package checksum")
	}
	if string(l2) != string(l1) {
		t.Fatal("limits group unexpectedly changed")
	}

	// New limits with the SAME old routes must also change the checksum.
	c3 := sampleContent()
	c3.Limits[0].RPM = 42
	r3, l3, err := c3.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if Checksum(r3, l3) == base {
		t.Fatal("changing limits must change the package checksum")
	}
	if string(r3) != string(r1) {
		t.Fatal("routes group unexpectedly changed")
	}

	// Identical content canonicalizes identically.
	r4, l4, err := sampleContent().CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if Checksum(r4, l4) != base {
		t.Fatal("identical content must yield the same checksum")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0", "1.0.0", 0},
		{"1.2", "1.10", -1},
		{"2.0", "1.99.99", 1},
		{"10.0", "9.0", 1},
		{"1.0.1", "1.0.0", 1},
	}
	for _, tc := range cases {
		got, err := CompareVersions(tc.a, tc.b)
		if err != nil {
			t.Fatalf("CompareVersions(%q,%q): %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Fatalf("CompareVersions(%q,%q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, bad := range []string{"", "1.x", "1..2", "v1.0"} {
		if _, err := CompareVersions(bad, "1.0"); err == nil {
			t.Fatalf("expected error for invalid version %q", bad)
		}
	}
}

func TestInCanaryBucketBoundaries(t *testing.T) {
	// 0% enrolls nobody; 100% enrolls everybody.
	for _, id := range []string{"a", "b", "client-42", "zzz"} {
		if InCanaryBucket(id, 0) {
			t.Fatalf("pct=0 must never enroll %q", id)
		}
		if !InCanaryBucket(id, 100) {
			t.Fatalf("pct=100 must always enroll %q", id)
		}
		if InCanaryBucket(id, -5) {
			t.Fatalf("negative pct must behave as 0 for %q", id)
		}
	}
}

func TestInCanaryBucketStableAndExact(t *testing.T) {
	// Determinism: same id + pct always maps the same way.
	const id = "stable-client-7"
	first := InCanaryBucket(id, 30)
	for i := 0; i < 50; i++ {
		if InCanaryBucket(id, 30) != first {
			t.Fatal("bucket assignment is not stable")
		}
	}

	// Exact bucket boundaries: if an id is in pct N it is in every pct > N.
	in := make(map[string]int)
	for n := 0; n < 400; n++ {
		cid := "client-" + string(rune('a'+n%26)) + "-" + itoa(n)
		bucket := bucketOf(cid)
		in[cid] = bucket
		for pct := 0; pct <= 100; pct++ {
			got := InCanaryBucket(cid, pct)
			want := bucket < pct
			if got != want {
				t.Fatalf("cid=%s bucket=%d pct=%d got=%v want=%v", cid, bucket, pct, got, want)
			}
		}
	}
}

func TestBucketDistributionRoughlyPct(t *testing.T) {
	const total = 5000
	count := 0
	for i := 0; i < total; i++ {
		if InCanaryBucket("dist-"+itoa(i), 25) {
			count++
		}
	}
	ratio := float64(count) / total
	if ratio < 0.22 || ratio > 0.28 {
		t.Fatalf("canary ratio %.3f outside [0.22,0.28]", ratio)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
