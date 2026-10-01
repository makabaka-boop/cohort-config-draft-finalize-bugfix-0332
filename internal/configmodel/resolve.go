package configmodel

import (
	"fmt"
)

// Snapshot pairs a release with the complete package it points at.
type Snapshot struct {
	Release *Release
	Package *Package
}

// ResolvedRelease is what the serving path returns to a client.
type ResolvedRelease struct {
	Release *Release
	Package *Package
	// Reason explains why this snapshot was selected.
	Reason string
	// Head is true when the client received the current head generation.
	Head bool
}

// Resolve picks one complete snapshot for a client request.
//
// snapshots must be ordered generation DESC (newest first). A generation is
// eligible for a client when:
//   - the client version satisfies its min_client_version, and
//   - rollout is 100%, or the client's stable hash bucket is selected.
//
// Walking newest-first means incompatible (too-new minimum version) or
// not-enrolled (partial rollout, client outside the canary bucket)
// generations are skipped toward the nearest compatible snapshot the client
// is actually enrolled in.
func Resolve(snapshots []Snapshot, clientVersion, clientID string, headGeneration int64) (*ResolvedRelease, error) {
	for _, sn := range snapshots {
		r := sn.Release
		cmp, err := CompareVersions(clientVersion, r.MinClientVersion)
		if err != nil {
			return nil, err
		}
		if cmp < 0 {
			continue // client too old for this generation
		}
		if r.RolloutPct < 100 && !InCanaryBucket(clientID, r.RolloutPct) {
			continue // outside this generation's stable canary group
		}
		head := r.Generation == headGeneration
		return &ResolvedRelease{
			Release: r,
			Package: sn.Package,
			Reason:  reasonFor(head, r),
			Head:    head,
		}, nil
	}
	return nil, nil // no compatible snapshot
}

func reasonFor(head bool, r *Release) string {
	switch {
	case head && r.RolloutPct >= 100:
		return "head"
	case head:
		return fmt.Sprintf("head_canary_%d", r.RolloutPct)
	case r.Action == "rollback":
		return "rollback_fallback"
	case r.RolloutPct < 100:
		return fmt.Sprintf("canary_%d_fallback", r.RolloutPct)
	default:
		return "compatible_fallback"
	}
}
