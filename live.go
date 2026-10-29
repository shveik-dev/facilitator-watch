package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type FacilitatorTarget struct{ ID, Name, Endpoint string }

// Source-owned bases include required API prefixes. No URL override is accepted.
func targets() []FacilitatorTarget {
	return []FacilitatorTarget{
		{"payai", "PayAI", "https://facilitator.payai.network"},
		{"mogami", "Mogami", "https://facilitator.mogami.tech"},
		{"dexter", "Dexter", "https://x402.dexter.cash"},
		{"solvador", "Solvador", "https://api.solvador.com"},
		{"polygon", "Polygon", "https://x402.polygon.technology"},
		{"near-x402", "NEAR x402", "https://x402.mikedotexe.com"},
		{"meridian", "Meridian", "https://api.mrdn.finance/v1"},
		{"casper", "Casper", "https://x402-facilitator.cspr.cloud"},
		{"coinbase-cdp", "Coinbase CDP", "https://api.cdp.coinbase.com/platform/v2/x402"},
	}
}

func targetByID(id string) (FacilitatorTarget, bool) {
	for _, t := range targets() {
		if t.ID == id {
			return t, true
		}
	}
	return FacilitatorTarget{}, false
}

// One connection retains connection-local pragmas. mu serializes compound
// reads and transactions. Never hold rows open across another query.
type liveStore struct {
	db            *sql.DB
	mu            sync.Mutex
	cursorKey     []byte
	cdpConfigured bool // Set at startup, before serving or collecting.
	schemes       schemeIndex
}

const schemaVersion = 2

// Fixed precision keeps SQLite's text ordering identical to UTC time ordering.
const storedTimeFormat = "2006-01-02T15:04:05.000000000Z"
const schemaV1 = `
CREATE TABLE facilitators(id TEXT PRIMARY KEY,name TEXT NOT NULL,endpoint TEXT NOT NULL);
CREATE TABLE checks(id INTEGER PRIMARY KEY,facilitator_id TEXT NOT NULL REFERENCES facilitators(id),checked_at TEXT NOT NULL,status TEXT NOT NULL,response_ms INTEGER NOT NULL,reachable INTEGER NOT NULL,http_status INTEGER,error TEXT NOT NULL);
CREATE INDEX checks_by_facilitator ON checks(facilitator_id,id DESC);
CREATE TABLE snapshots(check_id INTEGER PRIMARY KEY REFERENCES checks(id),raw BLOB NOT NULL,valid INTEGER NOT NULL);
CREATE TABLE events(id INTEGER PRIMARY KEY,check_id INTEGER NOT NULL REFERENCES checks(id),facilitator_id TEXT NOT NULL REFERENCES facilitators(id),at TEXT NOT NULL,facet_id TEXT,facet BLOB,basis TEXT NOT NULL,change_type TEXT NOT NULL,evidence BLOB NOT NULL);
CREATE INDEX events_by_facilitator ON events(facilitator_id,id);
CREATE INDEX events_by_facet ON events(facilitator_id,facet_id,id);
CREATE INDEX events_by_time ON events(facilitator_id,facet_id,at,id);
CREATE TABLE facet_misses(facilitator_id TEXT NOT NULL REFERENCES facilitators(id),facet_id TEXT NOT NULL,consecutive_misses INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(facilitator_id,facet_id));
PRAGMA user_version=1;`
const schemaV2 = `CREATE INDEX events_by_facilitator_time ON events(facilitator_id,at,id); PRAGMA user_version=2;`

func openLiveStore(ctx context.Context, path string) (*liveStore, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("a SQLite filesystem path is required")
	}
	// A literal absolute path cannot activate URI/DSN parameters.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ready := false
	defer func() {
		if !ready {
			db.Close()
		}
	}()
	for _, q := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err = db.ExecContext(ctx, q); err != nil {
			return nil, err
		}
	}
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return nil, err
	}
	if mode != "wal" {
		return nil, errors.New("SQLite WAL mode unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > schemaVersion {
		return nil, fmt.Errorf("database schema %d is newer than supported %d", version, schemaVersion)
	}
	if version == 0 {
		// Refuse existing unversioned development tables rather than guess at
		// compatibility or discard data. CREATE fails and rolls back safely.
		if _, err = tx.ExecContext(ctx, schemaV1); err != nil {
			return nil, fmt.Errorf("migration 1: %w", err)
		}
		version = 1
	}
	if version == 1 {
		if _, err = tx.ExecContext(ctx, schemaV2); err != nil {
			return nil, fmt.Errorf("migration 2: %w", err)
		}
	}
	for _, t := range targets() {
		if _, err = tx.ExecContext(ctx, `INSERT INTO facilitators VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,endpoint=excluded.endpoint`, t.ID, t.Name, t.Endpoint); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	cursorKey := make([]byte, 32)
	if _, err = rand.Read(cursorKey); err != nil {
		return nil, fmt.Errorf("initialize history cursor signer: %w", err)
	}
	ready = true
	return &liveStore{db: db, cursorKey: cursorKey}, nil
}

func (s *liveStore) Close() error { return s.db.Close() }

type pollResult struct {
	At         time.Time
	MS         int
	Reachable  bool
	HTTPStatus *int
	Error      string
	Raw        []byte
	Valid      bool
	RetryAfter time.Duration
}

func (s *liveStore) record(ctx context.Context, target FacilitatorTarget, r pollResult) error {
	fixed, ok := targetByID(target.ID)
	if !ok || fixed != target {
		return errors.New("unapproved target")
	}
	var snapshot Supported
	if r.Valid {
		var err error
		snapshot, err = parseSupported(r.Raw)
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "unavailable"
	if r.Valid {
		status = "operational"
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO checks(facilitator_id,checked_at,status,response_ms,reachable,http_status,error) VALUES(?,?,?,?,?,?,?)`, target.ID, r.At.UTC().Format(storedTimeFormat), status, r.MS, r.Reachable, r.HTTPStatus, r.Error)
	if err != nil {
		return err
	}
	checkID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if r.Raw != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO snapshots VALUES(?,?,?)`, checkID, r.Raw, r.Valid); err != nil {
			return err
		}
	}
	add := func(f *Facet, change, note string) error {
		var facetID any
		var fb []byte
		basis := "unknown"
		if f != nil {
			facetID, basis = f.ID, "declared"
			fb, err = json.Marshal(f)
			if err != nil {
				return err
			}
		}
		// Raw bytes live once in snapshots. Check IDs link every derived
		// facet to that snapshot without duplicating a large body per facet.
		eb, err := json.Marshal(Evidence{CheckID: fmt.Sprint(checkID), Source: target.Endpoint + "/supported", SourceType: "http_get", CapturedAt: r.At, Note: note})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO events(check_id,facilitator_id,at,facet_id,facet,basis,change_type,evidence) VALUES(?,?,?,?,?,?,?,?)`, checkID, target.ID, r.At.UTC().Format(storedTimeFormat), facetID, fb, basis, change, eb)
		return err
	}
	if !r.Valid {
		// A failure breaks the consecutive-success omission streak.
		if _, err = tx.ExecContext(ctx, `UPDATE facet_misses SET consecutive_misses=0 WHERE facilitator_id=?`, target.ID); err != nil {
			return err
		}
		if err = add(nil, "probe_failed", r.Error); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err = add(nil, "snapshot", "Successful GET /supported snapshot; declaration only, no payment testing."); err != nil {
		return err
	}
	current := map[string]bool{}
	for _, f := range supportedFacets(snapshot) {
		current[f.ID] = true
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM facet_misses WHERE facilitator_id=? AND facet_id=?`, target.ID, f.ID).Scan(&n); err != nil {
			return err
		}
		change := "confirmed"
		if n == 0 {
			change = "first_seen"
		}
		if err = add(&f, change, "Captured from GET /supported; declaration only."); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO facet_misses VALUES(?,?,0) ON CONFLICT(facilitator_id,facet_id) DO UPDATE SET consecutive_misses=0`, target.ID, f.ID); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.facet_id,e.facet FROM events e WHERE e.facilitator_id=? AND e.id=(SELECT max(p.id) FROM events p WHERE p.facilitator_id=e.facilitator_id AND p.facet_id=e.facet_id) AND e.facet_id IS NOT NULL`, target.ID)
	if err != nil {
		return err
	}
	prior := []Facet{}
	for rows.Next() {
		var id string
		var fb []byte
		if err = rows.Scan(&id, &fb); err != nil {
			rows.Close()
			return err
		}
		var f Facet
		if err = json.Unmarshal(fb, &f); err != nil {
			rows.Close()
			return err
		}
		if !current[id] {
			prior = append(prior, f)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, f := range prior {
		if _, err = tx.ExecContext(ctx, `UPDATE facet_misses SET consecutive_misses=min(consecutive_misses+1,3) WHERE facilitator_id=? AND facet_id=?`, target.ID, f.ID); err != nil {
			return err
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT consecutive_misses FROM facet_misses WHERE facilitator_id=? AND facet_id=?`, target.ID, f.ID).Scan(&n); err != nil {
			return err
		}
		change, note := "unknown", "Omitted from a successful snapshot; prior evidence is unconfirmed."
		if n >= 3 {
			change, note = "not_advertised", "Omitted from at least three consecutive successful snapshots; not provider withdrawal."
		}
		if err = add(&f, change, note); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func supportedFacets(s Supported) []Facet {
	m := map[string]Facet{}
	for _, k := range s.Kinds {
		// One facet per complete tuple: never combine independent lanes.
		b, _ := json.Marshal([]any{k.X402Version, k.Scheme, k.Network})
		id := fmt.Sprintf("kind-%x", sha256.Sum256(b))
		m[id] = Facet{id, "kind", fmt.Sprintf("v%d · %s · %s", k.X402Version, k.Scheme, k.Network), "Declared kinds[] tuple; extra retained in raw evidence"}
	}
	for _, x := range s.Extensions {
		id := fmt.Sprintf("extension-%x", sha256.Sum256([]byte(x)))
		m[id] = Facet{id, "extension", x, "Declared extension; behavior not tested"}
	}
	out := make([]Facet, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *liveStore) List(ctx context.Context) ([]Facilitator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Facilitator{}
	for _, target := range targets() {
		f, err := s.get(ctx, target)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *liveStore) Get(ctx context.Context, id string) (Facilitator, error) {
	target, ok := targetByID(id)
	if !ok {
		return Facilitator{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(ctx, target)
}

func (s *liveStore) get(ctx context.Context, target FacilitatorTarget) (Facilitator, error) {
	f := Facilitator{ID: target.ID, Name: target.Name, Endpoint: target.Endpoint, DataMode: "live", Supported: Supported{Kinds: []SupportedKind{}}, Capabilities: []Capability{}}
	f.Check.Status = "pending"
	if target.ID == "coinbase-cdp" && !s.cdpConfigured {
		f.ConfigurationNote = "Collection disabled: configure -cdp-key-file for the fixed Coinbase CDP GET /supported endpoint. Retained evidence, if any, is historical."
	}
	f.Check.Evidence.Note = "No check yet. No declaration evidence collected."
	var checked, failure string
	var checkID string
	var ms int
	var reachable bool
	var httpStatus sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,status,checked_at,response_ms,reachable,http_status,error FROM checks WHERE facilitator_id=? ORDER BY id DESC LIMIT 1`, f.ID).Scan(&checkID, &f.Check.Status, &checked, &ms, &reachable, &httpStatus, &failure)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}
	if err == nil {
		f.Check.CheckedAt, err = time.Parse(time.RFC3339Nano, checked)
		if err != nil {
			return f, err
		}
		f.Check.ResponseMS, f.Check.Reachable = &ms, &reachable
		if httpStatus.Valid {
			code := int(httpStatus.Int64)
			f.Check.HTTPStatus = &code
		}
		f.Check.Evidence = Evidence{CheckID: checkID, Source: target.Endpoint + "/supported", SourceType: "http_get", CapturedAt: f.Check.CheckedAt, Note: failure}
	}
	var raw []byte
	err = s.db.QueryRowContext(ctx, `SELECT c.id,c.checked_at,s.raw FROM snapshots s JOIN checks c ON c.id=s.check_id WHERE c.facilitator_id=? AND s.valid=1 ORDER BY c.id DESC LIMIT 1`, f.ID).Scan(&checkID, &checked, &raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}
	if err == nil {
		at, err := time.Parse(time.RFC3339Nano, checked)
		if err != nil {
			return f, err
		}
		f.Supported, err = parseSupported(raw)
		if err != nil {
			return f, err
		}
		f.SupportedEvidence = Evidence{CheckID: checkID, Source: target.Endpoint + "/supported", SourceType: "http_get", CapturedAt: at, Raw: raw, Note: "Last successful raw /supported response; declaration only."}
	}
	events, err := s.history(ctx, f.ID, true)
	if err != nil {
		return f, err
	}
	f.Capabilities = ProjectBySequence(events)
	return f, nil
}

func (s *liveStore) History(ctx context.Context, id string) ([]Event, error) {
	if _, ok := targetByID(id); !ok {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.history(ctx, id, false)
}

// Detail returns one consistent view for server-rendered profiles while a
// collector may be committing a newer check in the background.
func (s *liveStore) Detail(ctx context.Context, id string) (Facilitator, []Event, error) {
	target, ok := targetByID(id)
	if !ok {
		return Facilitator{}, nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.get(ctx, target)
	if err != nil {
		return f, nil, err
	}
	end := time.Now().UTC()
	start := end.Add(-7 * 24 * time.Hour)
	events, _, err := s.readHistoryPage(ctx, id, start, end, 100, "", 2<<20, true)
	return f, events, err
}

type historyCursor struct {
	ID          int64
	At          string
	High        int64
	Start, End  string
	Facilitator string
	MAC         string
}

func encodeHistoryCursor(c historyCursor, key []byte) string {
	c.MAC = ""
	unsigned, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(unsigned)
	c.MAC = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeHistoryCursor(raw string) (historyCursor, error) {
	var c historyCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) > 2048 {
		return c, ErrInvalidHistoryCursor
	}
	err = json.Unmarshal(b, &c)
	if err != nil || c.ID < 1 || c.High < c.ID {
		return c, ErrInvalidHistoryCursor
	}
	return c, nil
}

func verifyHistoryCursor(c historyCursor, key []byte) bool {
	got, err := base64.RawURLEncoding.DecodeString(c.MAC)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	c.MAC = ""
	unsigned, err := json.Marshal(c)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(unsigned)
	return hmac.Equal(got, mac.Sum(nil))
}

func (s *liveStore) HistoryPage(ctx context.Context, id string, start, end time.Time, limit int, cursor string) (HistoryPage, error) {
	if _, ok := targetByID(id); !ok {
		return HistoryPage{}, ErrNotFound
	}
	if limit < 1 || limit > 100 || end.Before(start) || end.Sub(start) > 7*24*time.Hour {
		return HistoryPage{}, errors.New("invalid history bounds")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, next, err := s.historyPage(ctx, id, start, end, limit, cursor, 2<<20)
	if err != nil {
		return HistoryPage{}, err
	}
	page := HistoryPage{Events: events, Start: start, End: end, Limit: limit}
	if next != "" {
		page.NextCursor = next
	}
	return page, nil
}

func (s *liveStore) historyPage(ctx context.Context, id string, start, end time.Time, limit int, token string, byteCap int) ([]Event, string, error) {
	return s.readHistoryPage(ctx, id, start, end, limit, token, byteCap, false)
}

// Latest slices scan backwards under the same budgets, then return chronological
// events for rendering. Public cursor pages always scan forwards.
func (s *liveStore) readHistoryPage(ctx context.Context, id string, start, end time.Time, limit int, token string, byteCap int, latest bool) ([]Event, string, error) {
	startS, endS := start.UTC().Format(storedTimeFormat), end.UTC().Format(storedTimeFormat)
	var c historyCursor
	if token == "" {
		if !latest {
			if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE facilitator_id=?`, id).Scan(&c.High); err != nil {
				return nil, "", err
			}
		}
		c = historyCursor{High: c.High, Start: startS, End: endS, Facilitator: id}
	} else {
		var err error
		c, err = decodeHistoryCursor(token)
		if err != nil || !verifyHistoryCursor(c, s.cursorKey) || c.Start != startS || c.End != endS || c.Facilitator != id {
			return nil, "", ErrInvalidHistoryCursor
		}
	}
	query := `SELECT e.id,e.at,e.facet,e.basis,e.change_type,e.evidence,CASE WHEN e.facet_id IS NULL THEN s.raw ELSE NULL END FROM events e LEFT JOIN snapshots s ON s.check_id=e.check_id WHERE e.facilitator_id=? AND e.id<=? AND e.at>=? AND e.at<? AND (e.at>? OR (e.at=? AND e.id>?)) ORDER BY e.at,e.id LIMIT ?`
	args := []any{id, c.High, startS, endS, c.At, c.At, c.ID, limit + 1}
	if latest {
		// The mutex already keeps this render consistent; no cursor high-water
		// scan is needed. The time index can stop after the newest limit+1 rows.
		query = `SELECT e.id,e.at,e.facet,e.basis,e.change_type,e.evidence,CASE WHEN e.facet_id IS NULL THEN s.raw ELSE NULL END FROM events e LEFT JOIN snapshots s ON s.check_id=e.check_id WHERE e.facilitator_id=? AND e.at>=? AND e.at<? ORDER BY e.at DESC,e.id DESC LIMIT ?`
		args = []any{id, startS, endS, limit + 1}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Event{}
	rawBytes := 0
	more := false
	last := c
	for rows.Next() {
		var e Event
		var rawAt string
		var fb, eb, raw []byte
		if err = rows.Scan(&e.ID, &rawAt, &fb, &e.Basis, &e.Change, &eb, &raw); err != nil {
			return nil, "", err
		}
		if len(out) >= limit {
			more = true
			break
		}
		if rawBytes+len(raw) > byteCap {
			if len(out) > 0 {
				more = true
				break
			}
			// Always return the first event; omit an over-budget raw body explicitly.
			raw = nil
			if json.Unmarshal(eb, &e.Evidence) == nil {
				e.Evidence.Note += " Raw body omitted: page raw-byte budget."
			}
		} else {
			rawBytes += len(raw)
			if err = json.Unmarshal(eb, &e.Evidence); err != nil {
				return nil, "", err
			}
		}
		e.At, err = time.Parse(time.RFC3339Nano, rawAt)
		if err != nil {
			return nil, "", err
		}
		if len(fb) > 0 {
			if err = json.Unmarshal(fb, &e.Facet); err != nil {
				return nil, "", err
			}
		}
		if json.Valid(raw) {
			e.Evidence.Raw = raw
		}
		e.DataMode = "live"
		out = append(out, e)
		idn, _ := strconv.ParseInt(e.ID, 10, 64)
		last.ID = idn
		last.At = rawAt
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	if latest {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out, "", nil
	}
	if more {
		return out, encodeHistoryCursor(last, s.cursorKey), nil
	}
	return out, "", nil
}

func (s *liveStore) history(ctx context.Context, id string, projection bool) ([]Event, error) {
	query := `SELECT e.id,e.at,e.facet,e.basis,e.change_type,e.evidence,CASE WHEN e.facet_id IS NULL THEN s.raw ELSE NULL END FROM events e LEFT JOIN snapshots s ON s.check_id=e.check_id WHERE e.facilitator_id=? ORDER BY e.id`
	args := []any{id}
	if projection {
		// Earliest/latest positives, latest facet state, latest failure are
		// sufficient for Project; no historical raw bodies are loaded here.
		query = `WITH facets AS (SELECT DISTINCT facet_id FROM events WHERE facilitator_id=? AND facet_id IS NOT NULL), wanted AS (
SELECT (SELECT id FROM events WHERE facilitator_id=? AND facet_id=f.facet_id AND change_type IN ('first_seen','confirmed') ORDER BY id LIMIT 1) AS id FROM facets f
UNION SELECT (SELECT id FROM events WHERE facilitator_id=? AND facet_id=f.facet_id AND change_type IN ('first_seen','confirmed') ORDER BY id DESC LIMIT 1) FROM facets f
UNION SELECT (SELECT id FROM events WHERE facilitator_id=? AND facet_id=f.facet_id ORDER BY id DESC LIMIT 1) FROM facets f
UNION SELECT (SELECT id FROM events WHERE facilitator_id=? AND facet_id=f.facet_id AND change_type='not_advertised' ORDER BY id DESC LIMIT 1) FROM facets f
UNION SELECT (SELECT id FROM events WHERE facilitator_id=? AND change_type='probe_failed' ORDER BY id DESC LIMIT 1))
SELECT id,at,facet,basis,change_type,evidence,NULL FROM events WHERE id IN (SELECT id FROM wanted) ORDER BY id`
		args = []any{id, id, id, id, id, id}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at string
		var fb, eb, raw []byte
		if err = rows.Scan(&e.ID, &at, &fb, &e.Basis, &e.Change, &eb, &raw); err != nil {
			return nil, err
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		if len(fb) > 0 {
			if err = json.Unmarshal(fb, &e.Facet); err != nil {
				return nil, err
			}
		}
		if err = json.Unmarshal(eb, &e.Evidence); err != nil {
			return nil, err
		}
		if json.Valid(raw) {
			e.Evidence.Raw = raw
		}
		e.DataMode = "live"
		out = append(out, e)
	}
	sortEvents(out)
	return out, rows.Err()
}

func (s *liveStore) Standards(ctx context.Context) ([]Standard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []Standard{
		{ID: "v1", Name: "x402 v1", Category: "protocol", Reference: "https://github.com/coinbase/x402/blob/main/specs/x402-specification-v1.md", Note: "The /supported list of kinds, with the older network identifiers kept as is."},
		{ID: "v2", Name: "x402 v2", Category: "protocol", Reference: "https://github.com/coinbase/x402/blob/main/specs/x402-specification-v2.md", Note: "The /supported list of kinds, extensions and signers. Exact fields are kept in the raw response."},
		{ID: "bazaar", Name: "Bazaar", Category: "extension", Reference: "https://github.com/coinbase/x402/blob/main/specs/extensions/bazaar.md", Note: "An optional discovery extension. Listing it doesn’t mean discovery works; we don’t check it."},
	}, nil
}

var _ Store = (*liveStore)(nil)
