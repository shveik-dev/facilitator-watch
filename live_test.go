package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const v2Snapshot = `{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:8453","extra":{"asset":"retained"}},{"x402Version":2,"scheme":"upto","network":"eip155:1"}],"extensions":["bazaar"],"signers":{},"future":{"retained":true}}`
const emptySnapshot = `{"kinds":[],"extensions":[],"signers":{}}`

func testStore(t *testing.T) *liveStore {
	t.Helper()
	s, err := openLiveStore(context.Background(), filepath.Join(t.TempDir(), "monitor.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func validResult(at time.Time, raw string) pollResult {
	code := 200
	return pollResult{At: at, MS: 12, Reachable: true, HTTPStatus: &code, Raw: []byte(raw), Valid: true}
}

func recordTest(t *testing.T, s *liveStore, r pollResult) {
	t.Helper()
	if err := s.record(context.Background(), targets()[0], r); err != nil {
		t.Fatal(err)
	}
}

func TestSQLitePersistenceAndSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitor ?mode=memory.sqlite")
	s, err := openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for pragma, want := range map[string]string{"journal_mode": "wal", "busy_timeout": "5000", "foreign_keys": "1", "user_version": "2"} {
		var got string
		if err = s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%s err=%v", pragma, got, err)
		}
	}
	recordTest(t, s, validResult(date("2026-10-01T00:00:00Z"), v2Snapshot))
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("configured path: %v", err)
	}
	s, err = openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err := s.Get(ctx, "payai")
	if err != nil {
		t.Fatal(err)
	}
	if string(f.SupportedEvidence.Raw) != v2Snapshot || len(f.Capabilities) != 3 || f.Check.Status != "operational" || f.Check.Reachable == nil || !*f.Check.Reachable {
		t.Fatalf("lost persistent evidence: %+v", f)
	}
	if len(f.Supported.Kinds) != 2 || string(f.Supported.Kinds[0].Extra) != `{"asset":"retained"}` || f.Supported.Kinds[1].Network != "eip155:1" {
		t.Fatal("tuple fields lost")
	}
	facets := supportedFacets(f.Supported)
	if len(facets) != 3 || facets[1].Category != "kind" {
		t.Fatal("tuple facet count")
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if newer, err := openLiveStore(ctx, path); err == nil {
		newer.Close()
		t.Fatal("opened newer schema")
	}
}

func TestSchemaV1MigratesToIndexedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	s, err := openLiveStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP INDEX events_by_facilitator_time; PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openLiveStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema migration version=%d err=%v", version, err)
	}
	var indexes int
	if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='events_by_facilitator_time'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("history index count=%d err=%v", indexes, err)
	}
}

func TestOmissionFailureAndReappearance(t *testing.T) {
	s := testStore(t)
	at := date("2026-10-01T00:00:00Z")
	recordTest(t, s, validResult(at, v2Snapshot))
	facet := supportedFacets(mustParse(t, v2Snapshot))[1]
	assertState := func(want string) Facilitator {
		t.Helper()
		f, err := s.Get(context.Background(), "payai")
		if err != nil {
			t.Fatal(err)
		}
		c := capability(t, f.Capabilities, facet.ID, "declared")
		if c.State != want || c.RemovedAt != nil || !c.FirstSeen.Equal(at) || c.Evidence.SourceType != "http_get" || c.Evidence.Fabricated {
			t.Fatalf("%s: %+v", want, c)
		}
		full, err := s.History(context.Background(), "payai")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.Capabilities, ProjectBySequence(full)) {
			t.Fatal("compact projection differs from full history")
		}
		return f
	}
	assertState("present")
	for i := 1; i <= 2; i++ {
		recordTest(t, s, validResult(at.Add(time.Duration(i)*time.Hour), emptySnapshot))
		assertState("stale")
	}
	// Failure must preserve last positive evidence and reset omission streak.
	recordTest(t, s, pollResult{At: at.Add(3 * time.Hour), MS: 8000, Error: "timeout"})
	f := assertState("stale")
	if f.Check.Status != "unavailable" || string(f.SupportedEvidence.Raw) != emptySnapshot {
		t.Fatal("last successful snapshot not preserved")
	}
	for i := 4; i <= 6; i++ {
		recordTest(t, s, validResult(at.Add(time.Duration(i)*time.Hour), emptySnapshot))
		want := "stale"
		if i == 6 {
			want = "not_advertised"
		}
		assertState(want)
	}
	recordTest(t, s, pollResult{At: at.Add(7 * time.Hour), MS: 8000, Error: "timeout"})
	assertState("not_advertised")
	recordTest(t, s, validResult(at.Add(7*time.Hour+30*time.Minute), emptySnapshot))
	assertState("not_advertised")
	recordTest(t, s, validResult(at.Add(8*time.Hour), v2Snapshot))
	f = assertState("present")
	if !f.SupportedEvidence.CapturedAt.Equal(at.Add(8 * time.Hour)) {
		t.Fatal("snapshot timestamp not refreshed")
	}
	events, err := s.History(context.Background(), "payai")
	if err != nil {
		t.Fatal(err)
	}
	var previous time.Time
	for _, e := range events {
		if e.At.Before(previous) || e.Change == "removed" || e.Basis == "observed" || e.Fabricated || e.DataMode != "live" {
			t.Fatalf("invalid history: %+v", e)
		}
		previous = e.At
	}
	chart := BuildChart(events, at.Add(9*time.Hour))
	for _, lane := range chart.Lanes {
		positive := 0
		for _, b := range lane.Bars {
			if b.Class != "uncertain" {
				positive++
			}
		}
		if positive != 2 {
			t.Fatalf("did not split observation gap: %+v", lane.Bars)
		}
	}
}

func TestSQLiteTransactionRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.record(ctx, targets()[0], validResult(time.Now(), v2Snapshot)); err == nil {
		t.Fatal("expected rollback")
	}
	for _, table := range []string{"checks", "snapshots", "events", "facet_misses"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s: %d %v", table, n, err)
		}
	}
}

func TestUnversionedDevelopmentSchemaIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.sqlite")
	s, err := openLiveStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if partial, err := openLiveStore(context.Background(), path); err == nil {
		partial.Close()
		t.Fatal("silently accepted partial schema")
	}
	// Resetting the marker lets the same data reopen: failed migration did
	// not drop or overwrite existing tables.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT count(*) FROM facilitators").Scan(&n); err != nil || n != len(targets()) {
		t.Fatal("partial data lost", err)
	}
}

func TestProjectionMatchesChronologicalHistoryAfterClockChange(t *testing.T) {
	s := testStore(t)
	first := validResult(date("2026-10-02T00:00:00Z"), v2Snapshot)
	recordTest(t, s, first)
	recordTest(t, s, pollResult{At: date("2026-10-01T00:00:00Z"), MS: 8000, Error: "timeout"})
	f, _, err := s.Detail(context.Background(), "payai")
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.History(context.Background(), "payai")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Capabilities, ProjectBySequence(events)) {
		t.Fatal("compact projection differs from durable event sequence")
	}
	for _, capability := range f.Capabilities {
		if capability.State != "stale" {
			t.Fatalf("later failure hidden by clock rollback: %+v", capability)
		}
	}
}

func TestHistoryPagesAreBoundedAndIgnoreLaterBackdatedEvents(t *testing.T) {
	s := testStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		recordTest(t, s, validResult(base.Add(time.Duration(i)*time.Minute), v2Snapshot))
	}
	start, end := base.Add(-time.Minute), base.Add(time.Hour)
	p1, err := s.HistoryPage(context.Background(), "payai", start, end, 2, "")
	if err != nil || len(p1.Events) != 2 || p1.NextCursor == "" {
		t.Fatalf("first page: %d %q %v", len(p1.Events), p1.NextCursor, err)
	}
	// A new event with an earlier timestamp cannot enter the captured traversal.
	recordTest(t, s, validResult(base.Add(-30*time.Second), v2Snapshot))
	p2, err := s.HistoryPage(context.Background(), "payai", start, end, 2, p1.NextCursor)
	if err != nil || len(p2.Events) == 0 {
		t.Fatalf("second page: %d %v", len(p2.Events), err)
	}
	for _, e := range p2.Events {
		if e.At.Before(p1.Events[len(p1.Events)-1].At) {
			t.Fatal("page chronology moved backwards")
		}
	}
	if _, err = s.HistoryPage(context.Background(), "mogami", start, end, 2, p1.NextCursor); err == nil {
		t.Fatal("cursor was not facilitator-bound")
	}
	tampered, err := decodeHistoryCursor(p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	tampered.High++
	unsigned, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	forged := base64.RawURLEncoding.EncodeToString(unsigned)
	if _, err = s.HistoryPage(context.Background(), "payai", start, end, 2, forged); err == nil {
		t.Fatal("tampered high-water cursor was accepted")
	}
}

func TestHistoryPageRawBodyBudgetAndDetailWindow(t *testing.T) {
	s := testStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		recordTest(t, s, validResult(base.Add(time.Duration(i)*time.Minute), v2Snapshot))
	}
	_, detail, err := s.Detail(context.Background(), "payai")
	if err != nil || len(detail) > 100 {
		t.Fatalf("detail was not bounded: %d %v", len(detail), err)
	}
	body := []byte(`"` + strings.Repeat("x", 900000) + `"`)
	if _, err := s.db.Exec(`UPDATE snapshots SET raw=?`, body); err != nil {
		t.Fatal(err)
	}
	start, end := base.Add(-time.Minute), base.Add(time.Hour)
	page, err := s.HistoryPage(context.Background(), "payai", start, end, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var rawBytes int
	for _, e := range page.Events {
		rawBytes += len(e.Evidence.Raw)
	}
	if rawBytes > 2<<20 || page.NextCursor == "" {
		t.Fatalf("raw budget/cursor: %d %q", rawBytes, page.NextCursor)
	}
}

func TestConcurrentStoreReadsAndWrites(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				if err := s.record(ctx, targets()[j%2], validResult(time.Now(), v2Snapshot)); err != nil {
					done <- err
					return
				}
				if _, err := s.List(ctx); err != nil {
					done <- err
					return
				}
				if _, err := s.History(ctx, "payai"); err != nil {
					done <- err
					return
				}
				f, events, err := s.Detail(ctx, "payai")
				if err != nil {
					done <- err
					return
				}
				if !reflect.DeepEqual(f.Capabilities, ProjectBySequence(events)) {
					done <- errors.New("inconsistent profile snapshot")
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("store deadlocked")
		}
	}
}

func mustParse(t *testing.T, raw string) Supported {
	t.Helper()
	s, err := parseSupported([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSupportedValidationAndTupleIdentity(t *testing.T) {
	for _, raw := range []string{v2Snapshot, emptySnapshot, `{"kinds":[{"x402Version":1,"scheme":"exact","network":"base"}]}`} {
		mustParse(t, raw)
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"kinds":null}`, `{"kinds":[null]}`, `{"kinds":[{"x402Version":2,"scheme":"exact","network":"base"}]}`, `{"kinds":[],"extensions":null}`, `{"kinds":[],"signers":[]}`, `{"kinds":[],"signers":{"eip155":null}}`, `{"kinds":[],"signers":{"eip155":[null]}}`, `{"kinds":[],"signers":{"eip155":[1]}}`, `{"kinds":[],"signers":{"eip155":[""]}}`, `{"kinds":[{"x402Version":1,"scheme":"","network":"base"}]}`} {
		if _, err := parseSupported([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	a := supportedFacets(Supported{Kinds: []SupportedKind{{1, "exact", "a:b", nil}, {2, "exact", "a-b", nil}, {2, "upto", "a:b", nil}}})
	if len(a) != 3 {
		t.Fatal("tuple collision")
	}
	b := supportedFacets(Supported{Kinds: []SupportedKind{{2, "upto", "a:b", nil}, {2, "exact", "a-b", nil}, {1, "exact", "a:b", nil}}})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("unstable tuple identities")
	}
}

func TestExactProtocolFieldsAndEntryBound(t *testing.T) {
	for _, raw := range []string{`{"Kinds":[]}`, `{"kinds":[{"X402Version":1,"scheme":"exact","network":"base"}]}`, `{"kinds":[],"extensions": null }`, `{"kinds":[{"x402Version":2,"scheme":"exact","network":"base"}],"Extensions":[],"Signers":{}}`} {
		if _, err := parseSupported([]byte(raw)); err == nil {
			t.Fatal("accepted malformed field names/null", raw)
		}
	}
	raw := `{"kinds":[],"extensions":[` + strings.Repeat(`"extension",`, 256) + `"last"]}`
	if _, err := parseSupported([]byte(raw)); err == nil {
		t.Fatal("entry bound missing")
	}
	s, err := parseSupported([]byte(`{"kinds":[],"Kinds":[{"x402Version":1,"scheme":"exact","network":"base"}]}`))
	if err != nil || len(s.Kinds) != 0 {
		t.Fatal("unknown casing must not override exact kinds[]", err)
	}
}

func TestSummaryReachabilityAndDeclarationValidity(t *testing.T) {
	s := testStore(t)
	code := 500
	recordTest(t, s, pollResult{At: time.Now(), MS: 20, Reachable: true, HTTPStatus: &code, Error: "HTTP 500"})
	items, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := summarize(items)
	if got.Unavailable != 1 || got.Pending != len(targets())-1 || got.Reachable != 1 || got.Operational != 0 || got.MedianResponseMS != nil {
		t.Fatalf("%+v", got)
	}
	recordTest(t, s, validResult(time.Now(), emptySnapshot))
	r := validResult(time.Now(), v2Snapshot)
	r.MS = 20
	if err = s.record(context.Background(), targets()[1], r); err != nil {
		t.Fatal(err)
	}
	items, err = s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got = summarize(items)
	if got.Operational != 2 || got.Reachable != 2 || got.Unavailable != 0 || got.MedianResponseMS == nil || *got.MedianResponseMS != 16 {
		t.Fatalf("%+v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type countingReader struct{ read int }

func (r *countingReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'x'
	}
	r.read += len(b)
	return len(b), nil
}

func TestCollectorCapsReadsAndClosesBody(t *testing.T) {
	reader := &countingReader{}
	body := &trackedBody{Reader: reader}
	client := newPollClient()
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	r, err := collect(context.Background(), targets()[0], client)
	if err != nil || r.Valid || len(r.Raw) != maxResponse || reader.read != maxResponse+1 || !body.closed {
		t.Fatalf("read bound/close failed: read=%d raw=%d closed=%v err=%v", reader.read, len(r.Raw), body.closed, err)
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestCollectorTransportPolicyAndFailures(t *testing.T) {
	client := newPollClient()
	transport := client.Transport.(*http.Transport)
	if client.Timeout != 8*time.Second || transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || !transport.DisableKeepAlives {
		t.Fatal("unsafe transport policy")
	}
	var calls int
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "GET" || req.URL.String() != targets()[0].Endpoint+"/supported" || req.Body != nil || req.Header.Get("User-Agent") != userAgent || req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
		}
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > 8*time.Second {
			t.Fatal("missing timeout")
		}
		r := mockResponse(302, "redirect body")
		r.Header.Set("Location", "https://unapproved.invalid/settle")
		return r, nil
	})
	r, err := collect(context.Background(), targets()[0], client)
	if err != nil || calls != 1 || r.Valid || !r.Reachable || *r.HTTPStatus != 302 {
		t.Fatalf("redirect followed: %+v %v calls=%d", r, err, calls)
	}
	if _, err = collect(context.Background(), FacilitatorTarget{"payai", "PayAI", "https://other.invalid"}, client); err == nil || calls != 1 {
		t.Fatal("accepted arbitrary target")
	}
	for _, tc := range []struct {
		name  string
		code  int
		body  string
		valid bool
	}{
		{"valid", 200, v2Snapshot, true}, {"empty", 200, emptySnapshot, true}, {"invalid", 200, `{"kinds":null}`, false}, {"error", 503, `{"error":"unavailable"}`, false}, {"oversize", 200, strings.Repeat("x", maxResponse+20), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return mockResponse(tc.code, tc.body), nil })
			r, err := collect(context.Background(), targets()[0], client)
			if err != nil || r.Valid != tc.valid || !r.Reachable || len(r.Raw) > maxResponse {
				t.Fatalf("%+v %v", r, err)
			}
			s := testStore(t)
			recordTest(t, s, r)
			f, err := s.Get(context.Background(), "payai")
			if err != nil || (f.Check.Status == "operational") != tc.valid {
				t.Fatalf("%+v %v", f, err)
			}
			var stored []byte
			if err := s.db.QueryRow("SELECT raw FROM snapshots").Scan(&stored); err != nil || string(stored) != string(r.Raw) {
				t.Fatal("raw failure/success body not retained", err)
			}
		})
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	r, err = collect(context.Background(), targets()[0], client)
	if err != nil || r.Valid || r.Reachable || r.HTTPStatus != nil || r.Error == "" {
		t.Fatalf("transport failure: %+v %v", r, err)
	}
}

func TestRetryPolicy(t *testing.T) {
	now := date("2026-10-01T00:00:00Z")
	for value, want := range map[string]time.Duration{"": 0, "invalid": 0, "-1": 0, "60": time.Minute, "999999999": maxRetryAfter, now.Add(2 * time.Hour).Format(http.TimeFormat): 2 * time.Hour, now.Add(-time.Hour).Format(http.TimeFormat): 0} {
		if got := retryAfter(value, now); got != want {
			t.Fatalf("Retry-After %s: %s want %s", value, got, want)
		}
	}
	for n, want := range []time.Duration{30 * time.Minute, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if got := nextPollDelay(n, 0); got != want {
			t.Fatalf("failure %d: %s", n, got)
		}
	}
	if nextPollDelay(1, 8*time.Hour) != maxRetryAfter || nextPollDelay(0, time.Hour) != pollInterval {
		t.Fatal("retry bound/success schedule")
	}
	client := newPollClient()
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		r := mockResponse(429, "busy")
		r.Header.Set("Retry-After", "7200")
		return r, nil
	})
	r, err := collect(context.Background(), targets()[0], client)
	if err != nil || r.RetryAfter != 2*time.Hour {
		t.Fatal("Retry-After not propagated", err)
	}
}

func TestCollectorOnce(t *testing.T) {
	for _, outcome := range []string{"success", "http failure", "transport failure"} {
		t.Run(outcome, func(t *testing.T) {
			s := testStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := make(chan string, len(targets()))
			client := newPollClient()
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests <- r.Method + " " + r.URL.String()
				switch outcome {
				case "http failure":
					resp := mockResponse(503, "busy")
					resp.Header.Set("Retry-After", "7200")
					return resp, nil
				case "transport failure":
					return nil, errors.New("mock transport failure")
				default:
					return mockResponse(200, emptySnapshot), nil
				}
			})
			done := make(chan struct{})
			go func() { defer close(done); runCollector(ctx, s, client, true) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("one-pass collector did not exit without cancellation")
			}
			close(requests)
			seen := map[string]int{}
			for request := range requests {
				seen[request]++
			}
			if len(seen) != len(targets())-1 {
				t.Fatalf("requests: %v", seen)
			}
			for _, target := range targets() {
				if target.ID == "coinbase-cdp" {
					var count int
					if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checks WHERE facilitator_id = ?", target.ID).Scan(&count); err != nil || count != 0 {
						t.Fatal("skipped CDP produced a check", err)
					}
					continue
				}
				if seen["GET "+target.Endpoint+"/supported"] != 1 {
					t.Fatalf("expected exactly one fixed GET: %v", seen)
				}
				var count int
				if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checks WHERE facilitator_id = ?", target.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("stored checks for %s: %d, %v", target.ID, count, err)
				}
			}
		})
	}
}

func TestCollectorImmediateBoundedAndCancellation(t *testing.T) {
	for _, once := range []bool{false, true} {
		t.Run(fmt.Sprintf("once=%t", once), func(t *testing.T) {
			testCollectorCancellation(t, once)
		})
	}
}

func testCollectorCancellation(t *testing.T, once bool) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 4)
	var active, maxActive atomic.Int32
	client := newPollClient()
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- r.URL.Host
		<-r.Context().Done()
		return nil, errors.New("cancelled mocked request")
	})
	done := make(chan struct{})
	key := integrationCDPKey(t)
	go func() { defer close(done); runCollector(ctx, s, client, once, key) }()
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case host := <-started:
			seen[host] = true
		case <-time.After(2 * time.Second):
			t.Fatal("initial poll not immediate")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector failed to join")
	}
	if len(seen) != 2 || maxActive.Load() != 2 || active.Load() != 0 || len(started) != 0 {
		t.Fatalf("targets=%v max=%d active=%d", seen, maxActive.Load(), active.Load())
	}
}
