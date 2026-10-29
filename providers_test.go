package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFixedProviderURLs(t *testing.T) {
	want := []struct{ id, url string }{
		{"payai", "https://facilitator.payai.network/supported"},
		{"mogami", "https://facilitator.mogami.tech/supported"},
		{"dexter", "https://x402.dexter.cash/supported"},
		{"solvador", "https://api.solvador.com/supported"},
		{"polygon", "https://x402.polygon.technology/supported"},
		{"near-x402", "https://x402.mikedotexe.com/supported"},
		{"meridian", "https://api.mrdn.finance/v1/supported"},
		{"casper", "https://x402-facilitator.cspr.cloud/supported"},
		{"coinbase-cdp", coinbaseSupportedURL},
	}
	if len(targets()) != len(want) {
		t.Fatal("target count")
	}
	for i, target := range targets() {
		if target.ID != want[i].id || target.Endpoint+"/supported" != want[i].url {
			t.Fatalf("unexpected target %d", i)
		}
	}
}

func integrationCDPKey(t *testing.T) *cdpKey {
	t.Helper()
	_, priv := syntheticCDPKey(t)
	key, err := parseCDPKey(syntheticCDPJSON(t, "integration-synthetic", priv))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestProviderAuthScopeAndSkip(t *testing.T) {
	key := integrationCDPKey(t)
	client := newPollClient()
	calls := 0
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if (req.Header.Get("Authorization") != "") != (req.URL.String() == coinbaseSupportedURL) {
			t.Error("authorization scope")
		}
		return mockResponse(200, emptySnapshot), nil
	})
	for _, target := range targets() {
		if r, err := collect(context.Background(), target, client, key); err != nil || !r.Valid {
			t.Fatalf("%s: %v", target.ID, err)
		}
	}
	cdp, _ := targetByID("coinbase-cdp")
	if _, err := collect(context.Background(), cdp, client); err == nil || calls != 9 {
		t.Fatal("unconfigured CDP attempted")
	}
	for _, base := range []string{cdp.Endpoint + "/extra", cdp.Endpoint + "?query=1", "https://api.cdp.coinbase.com"} {
		bad := cdp
		bad.Endpoint = base
		if _, err := collect(context.Background(), bad, client, key); err == nil || calls != 9 {
			t.Fatal("prefix escape")
		}
	}
	if key, err := collectionKey(false, filepath.Join(t.TempDir(), "does-not-exist")); key != nil || err != nil {
		t.Fatal("disabled collection read key")
	}
	if _, err := collectionKey(true, filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("invalid configuration accepted")
	}
}

// Captured from the anonymous Casper GET /supported on 2026-10-29; test input only.
const casperSupported = `{"kinds":[{"x402Version":2,"scheme":"exact","network":"casper:casper","extra":{"feePayer":"d8af1dc245e14a56157c171c614eb099d8aa7ea69f485fe6d48719123c90b033"}},{"x402Version":2,"scheme":"exact","network":"casper:casper-test","extra":{"feePayer":"81d557c9dcaadea97c34d79bf7b6af07aa9d760e5dd1aabf78a45fb39e072c3a"}}],"extensions":[],"signers":{"casper:*":["d8af1dc245e14a56157c171c614eb099d8aa7ea69f485fe6d48719123c90b033","81d557c9dcaadea97c34d79bf7b6af07aa9d760e5dd1aabf78a45fb39e072c3a"]}}`

func TestCasperAnonymousDeclaration(t *testing.T) {
	ctx := context.Background()
	target, ok := targetByID("casper")
	if !ok {
		t.Fatal("casper target missing")
	}
	client := newPollClient()
	calls := 0
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.String() != "https://x402-facilitator.cspr.cloud/supported" || req.Header.Get("Authorization") != "" {
			t.Error("unexpected Casper request")
		}
		return mockResponse(200, casperSupported), nil
	})
	r, err := collect(ctx, target, client)
	if err != nil || !r.Valid || calls != 1 || string(r.Raw) != casperSupported {
		t.Fatalf("casper collection: %v %+v", err, r)
	}
	s := mustParse(t, casperSupported)
	want := []string{"casper:casper", "casper:casper-test"}
	if len(s.Kinds) != len(want) || len(s.Extensions) != 0 || s.Signers == nil || len((*s.Signers)["casper:*"]) != 2 {
		t.Fatal("casper declaration shape")
	}
	for i, k := range s.Kinds {
		if k.X402Version != 2 || k.Scheme != "exact" || k.Network != want[i] || !strings.Contains(string(k.Extra), `"feePayer"`) {
			t.Fatalf("casper tuple %d", i)
		}
	}
	store := testStore(t)
	if err := store.record(ctx, target, r); err != nil {
		t.Fatal(err)
	}
	f, err := store.Get(ctx, "casper")
	if err != nil || f.Check.Status != "operational" || f.ConfigurationNote != "" || string(f.SupportedEvidence.Raw) != casperSupported || len(f.Capabilities) != 2 {
		t.Fatalf("casper evidence: %v", err)
	}
	for i, c := range f.Capabilities {
		if c.State != "present" || c.Facet.Category != "kind" || !strings.HasPrefix(c.Facet.Value, "v2 · exact · casper:casper") {
			t.Fatalf("casper capability %d: %+v", i, c.Facet)
		}
	}
}

func TestAllProvidersOnceGlobalCap(t *testing.T) {
	s := testStore(t)
	key := integrationCDPKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := newPollClient()
	var active, maximum atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		seen[req.URL.String()]++
		mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		return mockResponse(200, emptySnapshot), nil
	})
	runCollector(ctx, s, client, true, key)
	if ctx.Err() != nil || maximum.Load() != 2 || active.Load() != 0 || len(seen) != 9 {
		t.Fatal("global concurrency/once contract")
	}
	for _, target := range targets() {
		if seen[target.Endpoint+"/supported"] != 1 {
			t.Fatal("missing or duplicate fixed-target attempt")
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM checks").Scan(&count); err != nil || count != 9 {
		t.Fatal("missing completed checks", err)
	}
}

func TestProviderReopenPreservesOldEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.sqlite")
	s, err := openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.record(ctx, targets()[0], pollResult{At: time.Now().UTC(), Raw: []byte(v2Snapshot), Valid: true}); err != nil {
		t.Fatal(err)
	}
	// Recreate the original two-row metadata inventory, retaining real schema v2.
	if _, err := s.db.Exec("DELETE FROM facilitators WHERE id NOT IN ('payai','mogami')"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, err := s.List(ctx)
	if err != nil || len(items) != 9 {
		t.Fatal("new inventory", err)
	}
	for _, f := range items {
		if f.ID == "payai" {
			if string(f.SupportedEvidence.Raw) != v2Snapshot {
				t.Fatal("lost snapshot")
			}
			continue
		}
		if f.Check.Status != "pending" || !f.Check.CheckedAt.IsZero() || len(f.Capabilities) != 0 {
			t.Fatal("fabricated new evidence")
		}
		if f.ID == "coinbase-cdp" && f.ConfigurationNote == "" {
			t.Fatal("missing config note")
		}
	}
	if got := summarize(items); got.Total != 9 || got.Operational != 1 || got.Pending != 8 {
		t.Fatal("summary inventory")
	}
	cdp, _ := targetByID("coinbase-cdp")
	if err := s.record(ctx, cdp, pollResult{At: time.Now().UTC(), Raw: []byte(emptySnapshot), Valid: true}); err != nil {
		t.Fatal(err)
	}
	items, _ = s.List(ctx)
	if summarize(items).Operational != 1 {
		t.Fatal("disabled historic CDP counted operational")
	}
	if body := request(NewLiveServer(s), "GET", "/facilitators/coinbase-cdp").Body.String(); !strings.Contains(body, "configure -cdp-key-file") {
		t.Fatal("UI missing skip note")
	}
}

func TestCDPReflectedTokenAndErrorsNotRetained(t *testing.T) {
	target, _ := targetByID("coinbase-cdp")
	client := newPollClient()
	key := integrationCDPKey(t)
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return mockResponse(401, req.Header.Get("Authorization")), nil
	})
	r, err := collect(context.Background(), target, client, key)
	if err != nil || strings.Contains(string(r.Raw), "eyJ") || !strings.Contains(string(r.Raw), "[redacted]") {
		t.Fatal("reflected token retained")
	}
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: req.Header.Get("Authorization"), URL: "secret", Err: errors.New("secret")}
	})
	r, err = collect(context.Background(), target, client, key)
	if err != nil || r.Error != "GET /supported failed" {
		t.Fatal("transport secret retained")
	}
}

func TestRemovedTargetRetainedRowsStayPrivate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "removed.sqlite")
	s, err := openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	removed := FacilitatorTarget{"retired-provider", "Retired Provider", "https://retired.example"}
	at := time.Now().UTC()
	if err := s.record(ctx, targets()[0], validResult(at, v2Snapshot)); err != nil {
		t.Fatal(err)
	}
	// Model an old database without permitting removed targets through record().
	for _, q := range []string{
		`INSERT INTO facilitators VALUES('retired-provider','Retired Provider','https://retired.example')`,
		`INSERT INTO checks SELECT 9000,'retired-provider',checked_at,status,response_ms,reachable,http_status,error FROM checks WHERE id=1`,
		`INSERT INTO snapshots SELECT 9000,raw,valid FROM snapshots WHERE check_id=1`,
		`INSERT INTO events(check_id,facilitator_id,at,facet_id,facet,basis,change_type,evidence) SELECT 9000,'retired-provider',at,facet_id,facet,basis,change_type,evidence FROM events WHERE check_id=1`,
		`INSERT INTO facet_misses VALUES('retired-provider','synthetic-facet',2)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			s.Close()
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for q, want := range map[string]int{
		`PRAGMA user_version`: 2,
		`SELECT count(*) FROM facilitators WHERE id='retired-provider'`:                       1,
		`SELECT count(*) FROM checks WHERE facilitator_id='retired-provider'`:                 1,
		`SELECT count(*) FROM snapshots WHERE check_id=9000`:                                  1,
		`SELECT count(*) FROM events WHERE facilitator_id='retired-provider'`:                 4,
		`SELECT consecutive_misses FROM facet_misses WHERE facilitator_id='retired-provider'`: 2,
	} {
		var got int
		if err := s.db.QueryRowContext(ctx, q).Scan(&got); err != nil || got != want {
			t.Fatalf("retained state: got %d want %d: %v", got, want, err)
		}
	}
	if _, ok := targetByID(removed.ID); ok {
		t.Fatal("removed target still configured")
	}
	if _, err := s.Get(ctx, removed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed target readable", err)
	}
	if _, err := s.History(ctx, removed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed history readable", err)
	}
	if _, err := s.HistoryPage(ctx, removed.ID, at.Add(-time.Hour), at.Add(time.Hour), 10, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed history page readable", err)
	}
	server := NewLiveServer(s)
	for _, path := range []string{"/facilitators/retired-provider", "/api/v1/facilitators/retired-provider", "/api/v1/facilitators/retired-provider/history"} {
		if got := request(server, "GET", path); got.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, got.Code)
		}
	}
	for _, path := range []string{"/", "/api/v1/facilitators", "/api/v1/summary"} {
		got := request(server, "GET", path)
		if got.Code != http.StatusOK || strings.Contains(strings.ToLower(got.Body.String()), "retired-provider") {
			t.Fatalf("removed target exposed at %s", path)
		}
	}
	items, err := s.List(ctx)
	if err != nil || summarize(items).Total != 9 || summarize(items).Operational != 1 {
		t.Fatal("inventory or surviving evidence changed", err)
	}
	client := newPollClient()
	calls := 0
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return mockResponse(200, emptySnapshot), nil
	})
	if _, err := collect(ctx, removed, client); err == nil || calls != 0 {
		t.Fatal("removed target requested")
	}
	if err := s.record(ctx, removed, validResult(at, emptySnapshot)); err == nil {
		t.Fatal("removed target recorded")
	}
}
