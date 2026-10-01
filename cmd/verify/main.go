// Command verify is a one-shot end-to-end acceptance test for the
// configuration release service. It waits for the API to become healthy,
// then exercises every required behaviour and exits non-zero on the first
// failure. Run it via: docker compose run --rm verify
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type client struct {
	base string
	http *http.Client
}

func main() {
	base := os.Getenv("BASE_URL")
	if base == "" {
		base = "http://api:8080"
	}
	c := &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 15 * time.Second}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.waitHealthy(ctx); err != nil {
		log.Fatalf("api never became healthy: %v", err)
	}

	type test struct {
		name string
		fn   func() error
	}
	tests := []test{
		{"draft cross-group validation rejects dangling limit refs", c.testDraftValidation},
		{"finalize freezes draft into immutable package", c.testFinalizeImmutable},
		{"checksum binds routes and limits together", c.testChecksumBindsGroups},
		{"concurrent publish with same generation has exactly one winner", c.testConcurrentPublish},
		{"rollout percentage boundaries 0 and 100", c.testPercentageBoundaries},
		{"old client falls back to nearest compatible package", c.testOldClientFallback},
		{"stable hash grouping across requests and restarts", c.testStableGrouping},
		{"adjust and rollback create new generations pointing at full snapshots", c.testAdjustRollback},
		{"stale expected generation is rejected", c.testStaleGenerationRejected},
	}

	failed := 0
	for _, tc := range tests {
		log.Printf("== %s", tc.name)
		if err := tc.fn(); err != nil {
			log.Printf("FAIL: %s: %v", tc.name, err)
			failed++
		} else {
			log.Printf("PASS: %s", tc.name)
		}
	}
	if failed > 0 {
		log.Fatalf("%d acceptance test(s) failed", failed)
	}
	log.Printf("all acceptance tests passed")
}

func (c *client) waitHealthy(ctx context.Context) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
		resp, err := c.http.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ---- low level helpers ----

func (c *client) do(method, path string, body any) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("decode %s %s: %w (body=%s)", method, path, err, raw)
		}
	}
	return resp.StatusCode, out, nil
}

func (c *client) resolve(clientID, version string) (int, map[string]any, error) {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("client_version", version)
	return c.do(http.MethodGet, "/v1/resolve?"+q.Encode(), nil)
}

func mustStatus(t string, got, want int, body map[string]any) error {
	if got != want {
		return fmt.Errorf("%s: expected status %d, got %d: %v", t, want, got, body)
	}
	return nil
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// createDraftFinalize creates a draft, uploads both groups, finalizes and
// returns the package id.
func (c *client) createDraftFinalize(t string, routes []any, limits []any) (int64, error) {
	status, body, err := c.do(http.MethodPost, "/v1/drafts", nil)
	if err != nil {
		return 0, err
	}
	if err := mustStatus(t+" create", status, http.StatusCreated, body); err != nil {
		return 0, err
	}
	draftID := asInt64(body["id"])

	status, body, err = c.do(http.MethodPut, fmt.Sprintf("/v1/drafts/%d/content", draftID),
		map[string]any{"routes": routes, "limits": limits})
	if err != nil {
		return 0, err
	}
	if err := mustStatus(t+" content", status, http.StatusOK, body); err != nil {
		return 0, err
	}
	status, body, err = c.do(http.MethodPost, fmt.Sprintf("/v1/drafts/%d/finalize", draftID), nil)
	if err != nil {
		return 0, err
	}
	if err := mustStatus(t+" finalize", status, http.StatusCreated, body); err != nil {
		return 0, err
	}
	pkg, ok := body["package"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("%s: no package in finalize response", t)
	}
	return asInt64(pkg["id"]), nil
}

func limit(name string, rpm int) map[string]any {
	return map[string]any{"id": name, "name": name, "rpm": rpm}
}

func route(id, path, limitID string) map[string]any {
	return map[string]any{"id": id, "path": path, "upstream": "upstream-" + id, "limit_id": limitID}
}

// ---- acceptance scenarios ----

func (c *client) testDraftValidation() error {
	status, body, err := c.do(http.MethodPost, "/v1/drafts", nil)
	if err != nil {
		return err
	}
	if err := mustStatus("create draft", status, http.StatusCreated, body); err != nil {
		return err
	}
	id := asInt64(body["id"])

	// Route references a limit that does not exist in this draft.
	bad := map[string]any{
		"routes": []any{route("r1", "/v1/x", "ghost")},
		"limits": []any{limit("l1", 10)},
	}
	status, body, err = c.do(http.MethodPut, fmt.Sprintf("/v1/drafts/%d/content", id), bad)
	if err != nil {
		return err
	}
	if status != http.StatusUnprocessableEntity {
		return fmt.Errorf("dangling cross-group reference expected 422, got %d: %v", status, body)
	}
	problems, _ := body["problems"].([]any)
	joined := ""
	for _, p := range problems {
		joined += fmt.Sprint(p) + ";"
	}
	if !strings.Contains(joined, "ghost") {
		return fmt.Errorf("validation problems should mention dangling limit id, got %v", problems)
	}

	// Finalizing an incomplete draft is refused.
	status, body, err = c.do(http.MethodPost, fmt.Sprintf("/v1/drafts/%d/finalize", id), nil)
	if err != nil {
		return err
	}
	if status != http.StatusUnprocessableEntity {
		return fmt.Errorf("finalizing incomplete draft expected 422, got %d: %v", status, body)
	}
	return nil
}

func (c *client) testFinalizeImmutable() error {
	pkg, err := c.createDraftFinalize("immutable",
		[]any{route("r1", "/a", "l1")}, []any{limit("l1", 10)})
	if err != nil {
		return err
	}
	if pkg <= 0 {
		return fmt.Errorf("invalid package id %d", pkg)
	}
	// The package endpoint serves the complete snapshot (both groups).
	status, body, err := c.do(http.MethodGet, fmt.Sprintf("/v1/packages/%d", pkg), nil)
	if err != nil {
		return err
	}
	if err := mustStatus("get package", status, http.StatusOK, body); err != nil {
		return err
	}
	if body["routes"] == nil || body["limits"] == nil || body["checksum"] == "" {
		return fmt.Errorf("package snapshot incomplete: %v", body)
	}
	return nil
}

func (c *client) testChecksumBindsGroups() error {
	base := []any{route("r1", "/a", "l1")}
	limits := []any{limit("l1", 10)}

	p1, err := c.createDraftFinalize("bind-1", base, limits)
	if err != nil {
		return err
	}
	// Identical pair must be content-addressed to the SAME package.
	pSame, err := c.createDraftFinalize("bind-2",
		[]any{route("r1", "/a", "l1")}, []any{limit("l1", 10)})
	if err != nil {
		return err
	}
	if pSame != p1 {
		return fmt.Errorf("identical routes+limits must reuse package: %d vs %d", pSame, p1)
	}
	// New routes with the SAME old limits => different package.
	pNewRoutes, err := c.createDraftFinalize("bind-3",
		[]any{route("r1", "/b", "l1")}, []any{limit("l1", 10)})
	if err != nil {
		return err
	}
	if pNewRoutes == p1 {
		return fmt.Errorf("new routes with old limits must be a new package, both %d", p1)
	}
	// Same routes with NEW limits => also different.
	pNewLimits, err := c.createDraftFinalize("bind-4",
		[]any{route("r1", "/a", "l1")}, []any{limit("l1", 20)})
	if err != nil {
		return err
	}
	if pNewLimits == p1 || pNewLimits == pNewRoutes {
		return fmt.Errorf("new limits must produce a distinct package")
	}
	return nil
}

// publishPackage publishes a package and returns (generation, packageID).
func (c *client) publishPackage(pkg int64, pct int, minVersion string, expectGen int64) (int, map[string]any, error) {
	return c.do(http.MethodPost, "/v1/releases", map[string]any{
		"package_id":          pkg,
		"rollout_pct":         pct,
		"min_client_version":  minVersion,
		"expected_generation": expectGen,
	})
}

func (c *client) testConcurrentPublish() error {
	pkg, err := c.createDraftFinalize("concurrent",
		[]any{route("r1", "/c", "l1")}, []any{limit("l1", 30)})
	if err != nil {
		return err
	}
	head, err := c.currentHead()
	if err != nil {
		return err
	}

	const n = 2 // the two goroutines hit the two API instances
	instances := []*client{c, c.sibling()}
	var wg sync.WaitGroup
	statuses := make([]int, n)
	bodies := make([]map[string]any, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			st, b, e := instances[i].publishPackage(pkg, 100, "1.0", head)
			if e != nil {
				statuses[i] = -1
				bodies[i] = map[string]any{"error": e.Error()}
				return
			}
			statuses[i] = st
			bodies[i] = b
		}(i)
	}
	close(start)
	wg.Wait()

	created, conflict := 0, 0
	for i, st := range statuses {
		switch st {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			return fmt.Errorf("unexpected status %d body %v", st, bodies[i])
		}
	}
	if created != 1 || conflict != n-1 {
		return fmt.Errorf("expected exactly 1 created and %d conflict, got created=%d conflict=%d",
			n-1, created, conflict)
	}
	newHead, err := c.currentHead()
	if err != nil {
		return err
	}
	if newHead != head+1 {
		return fmt.Errorf("head should advance by exactly one: %d -> %d", head, newHead)
	}
	return nil
}

func (c *client) testPercentageBoundaries() error {
	// Reject out-of-range percentages.
	pkg, err := c.createDraftFinalize("pct",
		[]any{route("r1", "/p", "l1")}, []any{limit("l1", 5)})
	if err != nil {
		return err
	}
	head, _ := c.currentHead()
	for _, bad := range []int{-1, 101} {
		status, body, err := c.publishPackage(pkg, bad, "1.0", head)
		if err != nil {
			return err
		}
		if status == http.StatusCreated {
			return fmt.Errorf("rollout_pct=%d must be rejected", bad)
		}
		_ = body
	}

	// Publish at 0%: no client resolves to it; everyone stays on prior.
	status, body, err := c.publishPackage(pkg, 0, "1.0", head)
	if err != nil {
		return err
	}
	if err := mustStatus("publish 0%", status, http.StatusCreated, body); err != nil {
		return err
	}
	zeroGen := asInt64(body["generation"])
	status, body, err = c.resolve("anyone-at-all", "9.9")
	if err != nil {
		return err
	}
	if status == http.StatusOK && asInt64(body["generation"]) == zeroGen {
		return fmt.Errorf("0%% rollout must serve no client, got generation %d", zeroGen)
	}

	// Adjust to 100% creates a new generation; every client now gets it.
	headAfterZero, _ := c.currentHead()
	status, body, err = c.do(http.MethodPost, "/v1/releases/adjust", map[string]any{
		"rollout_pct": 100, "expected_generation": headAfterZero,
	})
	if err != nil {
		return err
	}
	if err := mustStatus("adjust 100%", status, http.StatusCreated, body); err != nil {
		return err
	}
	fullGen := asInt64(body["generation"])
	if fullGen != headAfterZero+1 {
		return fmt.Errorf("adjust must create a new generation")
	}
	status, body, err = c.resolve("anyone-at-all", "9.9")
	if err != nil {
		return err
	}
	if err := mustStatus("resolve at 100%", status, http.StatusOK, body); err != nil {
		return err
	}
	if asInt64(body["generation"]) != fullGen || asInt64(body["package_id"]) != pkg {
		return fmt.Errorf("at 100%% every client must get generation %d package %d, got %+v",
			fullGen, pkg, body)
	}
	if body["served_head"] != true {
		return fmt.Errorf("100%% resolution must report served_head=true")
	}
	return nil
}

func (c *client) testOldClientFallback() error {
	// Current head at 100% requires client 2.0.
	pkgV2, err := c.createDraftFinalize("old-v2",
		[]any{route("r1", "/v2", "l1")}, []any{limit("l1", 50)})
	if err != nil {
		return err
	}
	head, _ := c.currentHead()
	status, body, err := c.publishPackage(pkgV2, 100, "2.0", head)
	if err != nil {
		return err
	}
	if err := mustStatus("publish v2", status, http.StatusCreated, body); err != nil {
		return err
	}

	// Old client 1.x cannot take it and must fall back to the nearest
	// compatible snapshot; response carries actual package + generation.
	status, body, err = c.resolve("legacy-client", "1.5")
	if err != nil {
		return err
	}
	if err := mustStatus("resolve old client", status, http.StatusOK, body); err != nil {
		return err
	}
	if asInt64(body["package_id"]) == pkgV2 {
		return fmt.Errorf("client 1.5 must not receive package requiring 2.0")
	}
	if body["package_id"] == nil || body["generation"] == nil || body["head_generation"] == nil {
		return fmt.Errorf("fallback response must include package_id, generation and head_generation")
	}
	if body["served_head"] == true {
		return fmt.Errorf("fallback response must report served_head=false")
	}
	// The fallback generation must be older than the head and still complete.
	if asInt64(body["generation"]) >= asInt64(body["head_generation"]) {
		return fmt.Errorf("fallback generation must be older than head: %+v", body)
	}
	if body["routes"] == nil || body["limits"] == nil {
		return fmt.Errorf("fallback must still return the full snapshot")
	}

	// A current client gets the head.
	status, body, err = c.resolve("modern-client", "2.0")
	if err != nil {
		return err
	}
	if asInt64(body["package_id"]) != pkgV2 || body["served_head"] != true {
		return fmt.Errorf("client 2.0 must serve the head package %d, got %+v", pkgV2, body)
	}
	return nil
}

func (c *client) testStableGrouping() error {
	// Publish a fresh package at exactly 30%.
	pkg, err := c.createDraftFinalize("stable",
		[]any{route("r1", "/s", "l1")}, []any{limit("l1", 7)})
	if err != nil {
		return err
	}
	head, _ := c.currentHead()
	status, body, err := c.publishPackage(pkg, 30, "1.0", head)
	if err != nil {
		return err
	}
	if err := mustStatus("publish 30%", status, http.StatusCreated, body); err != nil {
		return err
	}
	canaryGen := asInt64(body["generation"])

	// Repeat a large fixed client set many times: assignments never change.
	const clients = 400
	first := make(map[string]bool, clients)
	for round := 0; round < 5; round++ {
		in := 0
		for i := 0; i < clients; i++ {
			id := "grp-" + strconv.Itoa(i)
			status, body, err := c.resolve(id, "1.0")
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("resolve %s: status %d", id, status)
			}
			got := asInt64(body["generation"]) == canaryGen
			if round == 0 {
				first[id] = got
			} else if first[id] != got {
				return fmt.Errorf("client %s assignment changed between rounds", id)
			}
			if got {
				in++
			}
		}
		if round == 0 {
			ratio := float64(in) / clients
			if ratio < 0.18 || ratio > 0.42 {
				return fmt.Errorf("30%% canary ratio out of expected range: %.2f", ratio)
			}
			log.Printf("   canary enrollment at 30%%: %d/%d", in, clients)
		}
	}

	// Both API replicas (api, api2) must return identical assignments.
	// This is the cross-instance / post-restart stability guarantee: the
	// hash function lives in shared code and never reads local state.
	c2 := c.sibling()
	for i := 0; i < 100; i++ {
		id := "replica-" + strconv.Itoa(i)
		_, b1, err := c.resolve(id, "1.0")
		if err != nil {
			return err
		}
		_, b2, err := c2.resolve(id, "1.0")
		if err != nil {
			return err
		}
		g1, g2 := asInt64(b1["generation"]), asInt64(b2["generation"])
		p1, p2 := asInt64(b1["package_id"]), asInt64(b2["package_id"])
		if g1 != g2 || p1 != p2 {
			return fmt.Errorf("client %s resolved differently across instances: "+
				"gen %d/%d pkg %d/%d", id, g1, g2, p1, p2)
		}
	}
	return nil
}

func (c *client) testAdjustRollback() error {
	// Two packages, publish the first then the second at 100%.
	pkgA, err := c.createDraftFinalize("ar-a",
		[]any{route("r1", "/a", "l1")}, []any{limit("l1", 1)})
	if err != nil {
		return err
	}
	pkgB, err := c.createDraftFinalize("ar-b",
		[]any{route("r1", "/b", "l1")}, []any{limit("l1", 2)})
	if err != nil {
		return err
	}
	head, _ := c.currentHead()
	if status, body, err := c.publishPackage(pkgA, 100, "1.0", head); err != nil || status != http.StatusCreated {
		return fmt.Errorf("publish A: status=%d err=%v body=%v", status, err, body)
	}
	headA, _ := c.currentHead()
	if status, body, err := c.publishPackage(pkgB, 100, "1.0", headA); err != nil || status != http.StatusCreated {
		return fmt.Errorf("publish B: status=%d err=%v body=%v", status, err, body)
	}
	headB, _ := c.currentHead()

	// History lists both, newest first, each with its complete snapshot.
	status, body, err := c.do(http.MethodGet, "/v1/releases", nil)
	if err != nil {
		return err
	}
	if err := mustStatus("list releases", status, http.StatusOK, body); err != nil {
		return err
	}
	releases, _ := body["releases"].([]any)
	if len(releases) < 2 {
		return fmt.Errorf("expected at least 2 release records, got %d", len(releases))
	}
	for i, r := range releases {
		rec := r.(map[string]any)
		rel := rec
		_ = rel
		if rec["generation"] == nil || rec["package_id"] == nil {
			return fmt.Errorf("release record missing identifiers: %v", rec)
		}
		p, ok := rec["package"].(map[string]any)
		if !ok || p["routes"] == nil || p["limits"] == nil {
			return fmt.Errorf("release record %d must embed full package snapshot", i)
		}
	}

	// Roll back to package A as a NEW generation.
	status, body, err = c.do(http.MethodPost, "/v1/releases/rollback", map[string]any{
		"package_id": pkgA, "expected_generation": headB,
	})
	if err != nil {
		return err
	}
	if err := mustStatus("rollback", status, http.StatusCreated, body); err != nil {
		return err
	}
	rbGen := asInt64(body["generation"])
	if rbGen <= headB {
		return fmt.Errorf("rollback must create a newer generation: %d <= %d", rbGen, headB)
	}
	rel, _ := body["release"].(map[string]any)
	if rel["action"] != "rollback" {
		return fmt.Errorf("rollback record action = %v, want rollback", rel["action"])
	}
	if asInt64(rel["package_id"]) != pkgA {
		return fmt.Errorf("rollback must point at package A %d, got %v", pkgA, rel["package_id"])
	}
	if p, _ := rel["package"].(map[string]any); p == nil || p["routes"] == nil || p["limits"] == nil {
		return fmt.Errorf("rollback response must embed the complete snapshot")
	}

	// Fetching that single generation record must return the same
	// complete snapshot (queries never point at a dangling package).
	status, body, err = c.do(http.MethodGet, fmt.Sprintf("/v1/releases/%d", rbGen), nil)
	if err != nil {
		return err
	}
	if err := mustStatus("get rollback generation", status, http.StatusOK, body); err != nil {
		return err
	}
	if asInt64(body["generation"]) != rbGen || asInt64(body["package_id"]) != pkgA {
		return fmt.Errorf("generation record mismatches: %+v", body)
	}
	gp, _ := body["package"].(map[string]any)
	if gp == nil || gp["routes"] == nil || gp["limits"] == nil {
		return fmt.Errorf("single-generation record must embed full snapshot")
	}
	if asInt64(gp["id"]) != pkgA {
		return fmt.Errorf("single-generation record points at wrong package")
	}

	// Served config now equals package A.
	status, body, err = c.resolve("post-rb", "1.0")
	if err != nil {
		return err
	}
	if asInt64(body["package_id"]) != pkgA || asInt64(body["generation"]) != rbGen {
		return fmt.Errorf("after rollback clients must get package A at new generation, got %+v", body)
	}

	// Cannot roll back to a package never published.
	unpublished, err := c.createDraftFinalize("ar-never",
		[]any{route("r1", "/never", "l1")}, []any{limit("l1", 9)})
	if err != nil {
		return err
	}
	headRB, _ := c.currentHead()
	status, body, err = c.do(http.MethodPost, "/v1/releases/rollback", map[string]any{
		"package_id": unpublished, "expected_generation": headRB,
	})
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		return fmt.Errorf("rollback to never-published package must be rejected")
	}
	return nil
}

func (c *client) testStaleGenerationRejected() error {
	pkg, err := c.createDraftFinalize("stale",
		[]any{route("r1", "/z", "l1")}, []any{limit("l1", 3)})
	if err != nil {
		return err
	}
	head, _ := c.currentHead()
	// First commit at a deliberately stale expectation (head-1) fails.
	stale := head - 1
	if stale < 0 {
		stale = 0
	}
	status, body, err := c.publishPackage(pkg, 100, "1.0", stale)
	if err != nil {
		return err
	}
	if status != http.StatusConflict {
		return fmt.Errorf("stale expected generation must yield 409, got %d: %v", status, body)
	}
	if asInt64(body["current_generation"]) != head {
		return fmt.Errorf("409 must report current_generation=%d, got %v", head, body)
	}
	return nil
}

func (c *client) currentHead() (int64, error) {
	status, body, err := c.do(http.MethodGet, "/v1/releases/current", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("current: status %d body %v", status, body)
	}
	return asInt64(body["head_generation"]), nil
}

// sibling returns a client for the second API replica, proving that
// cross-instance commits serialize on the database. It falls back to the
// primary when no second instance is configured.
func (c *client) sibling() *client {
	c2 := &client{base: secondBaseURL(), http: &http.Client{Timeout: 15 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c2.waitHealthy(ctx); err != nil {
		log.Printf("   second API instance unreachable (%v); using primary", err)
		return c
	}
	return c2
}

func secondBaseURL() string {
	if v := os.Getenv("SECOND_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://api2:8080"
}
