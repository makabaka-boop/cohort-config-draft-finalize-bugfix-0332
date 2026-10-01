// Package configmodel holds the domain types for route/limit configuration
// packages: validated drafts, immutable packages, and the release history.
package configmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// Route is one entry of a site's route configuration.
type Route struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Upstream string `json:"upstream,omitempty"`
	// LimitID optionally references an entry in the same package's limits
	// group. The reference is validated across groups at finalize time.
	LimitID string `json:"limit_id,omitempty"`
}

// Limit is one entry of a site's rate-limit configuration.
type Limit struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	RPM   int    `json:"rpm"`
	Burst int    `json:"burst,omitempty"`
}

// DraftContent is the editable pair of groups. Routes and limits are always
// stored as canonical (re-marshaled) JSON so checksums are deterministic.
type DraftContent struct {
	Routes []Route `json:"routes"`
	Limits []Limit `json:"limits"`
}

// Validate checks both groups internally and the cross-group references:
// every route.limit_id must point at a limit id present in the same draft.
// It returns a list of human-readable problems (empty when valid).
func (c *DraftContent) Validate() []string {
	var problems []string

	if len(c.Routes) == 0 {
		problems = append(problems, "routes must contain at least one entry")
	}
	routeIDs := make(map[string]bool, len(c.Routes))
	for i, r := range c.Routes {
		if r.ID == "" {
			problems = append(problems, fmt.Sprintf("routes[%d].id must not be empty", i))
		} else if routeIDs[r.ID] {
			problems = append(problems, fmt.Sprintf("duplicate route id %q", r.ID))
		}
		routeIDs[r.ID] = true
		if r.Path == "" {
			problems = append(problems, fmt.Sprintf("routes[%d].path must not be empty", i))
		}
	}

	if len(c.Limits) == 0 {
		problems = append(problems, "limits must contain at least one entry")
	}
	limitIDs := make(map[string]bool, len(c.Limits))
	for i, l := range c.Limits {
		if l.ID == "" {
			problems = append(problems, fmt.Sprintf("limits[%d].id must not be empty", i))
		} else if limitIDs[l.ID] {
			problems = append(problems, fmt.Sprintf("duplicate limit id %q", l.ID))
		}
		limitIDs[l.ID] = true
		if l.RPM < 0 {
			problems = append(problems, fmt.Sprintf("limits[%d].rpm must not be negative", i))
		}
	}

	for i, r := range c.Routes {
		if r.LimitID != "" && !limitIDs[r.LimitID] {
			problems = append(problems, fmt.Sprintf(
				"routes[%d].limit_id %q has no matching limits entry", i, r.LimitID))
		}
	}
	return problems
}

// CanonicalJSON re-marshals the two groups with stable key ordering and
// no whitespace. All stored payloads and checksums derive from this form,
// so byte-identical content always yields the same checksum.
func (c *DraftContent) CanonicalJSON() (routes []byte, limits []byte, err error) {
	if routes, err = json.Marshal(c.Routes); err != nil {
		return nil, nil, fmt.Errorf("canonicalize routes: %w", err)
	}
	if limits, err = json.Marshal(c.Limits); err != nil {
		return nil, nil, fmt.Errorf("canonicalize limits: %w", err)
	}
	return routes, limits, nil
}

// Checksum hashes the two groups together so a package is only identical to
// another package when BOTH groups match (never new routes + old limits).
func Checksum(routesCanonical, limitsCanonical []byte) string {
	h := sha256.New()
	h.Write([]byte("routes:"))
	h.Write(routesCanonical)
	h.Write([]byte("\nlimits:"))
	h.Write(limitsCanonical)
	return hex.EncodeToString(h.Sum(nil))
}

// Package is an immutable, cross-group-validated snapshot. The database
// additionally forbids UPDATE/DELETE on packages rows.
type Package struct {
	ID        int64           `json:"id"`
	Checksum  string          `json:"checksum"`
	Routes    json.RawMessage `json:"routes"`
	Limits    json.RawMessage `json:"limits"`
	CreatedAt time.Time       `json:"created_at"`
}

// Draft is the working copy a client edits before finalizing.
type Draft struct {
	ID        int64           `json:"id"`
	Status    string          `json:"status"`
	Routes    json.RawMessage `json:"routes,omitempty"`
	Limits    json.RawMessage `json:"limits,omitempty"`
	PackageID *int64          `json:"package_id,omitempty"`
	Checksum  string          `json:"checksum,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Release is one generation of publication history. Each row points at a
// complete package snapshot; adjustments and rollbacks insert new rows.
type Release struct {
	Generation       int64     `json:"generation"`
	PackageID        int64     `json:"package_id"`
	RolloutPct       int       `json:"rollout_pct"`
	MinClientVersion string    `json:"min_client_version"`
	Action           string    `json:"action"`
	Note             string    `json:"note,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	Package          *Package  `json:"package,omitempty"`
}

// DecodeStrict parses JSON into v, rejecting unknown fields and trailing data.
func DecodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing JSON content")
	}
	return nil
}

// CompareVersions compares dotted numeric versions (e.g. "1.2", "1.10.3").
// Missing components count as zero, so "1.2" == "1.2.0". Returns -1, 0, 1.
// Non-numeric or empty components make the version invalid.
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", a, err)
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", b, err)
	}
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		va, vb := 0, 0
		if i < len(pa) {
			va = pa[i]
		}
		if i < len(pb) {
			vb = pb[i]
		}
		if va != vb {
			if va < vb {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(v string) ([]int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, fmt.Errorf("empty version")
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("empty component")
		}
		n := 0
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return nil, fmt.Errorf("non-numeric component %q", p)
			}
			n = n*10 + int(ch-'0')
		}
		out = append(out, n)
	}
	return out, nil
}

// InCanaryBucket deterministically assigns a client id to one of 100 hash
// buckets (0-99) using FNV-1a. The mapping is identical on every API
// instance and across restarts, which is what makes rollout groups stable.
func InCanaryBucket(clientID string, pct int) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return bucketOf(clientID) < pct
}

// bucketOf returns the stable 0-99 bucket for a client id.
func bucketOf(clientID string) int {
	h := fnv.New64a()
	h.Write([]byte(clientID))
	return int(h.Sum64() % 100)
}
