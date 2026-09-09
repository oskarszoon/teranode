package replayrecovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
)

var (
	ErrIncomplete = sentinelError("recovery audit contains unresolved records")
	ErrPending    = sentinelError("recovery verification pending")
)

type Summary struct {
	Scanned    int64  `json:"scanned"`
	FullySpent int64  `json:"fully_spent"`
	Live       int64  `json:"live"`
	Unknown    int64  `json:"unknown"`
	Blocked    int64  `json:"blocked"`
	Repaired   int64  `json:"repaired"`
	Stage      string `json:"stage"`
}

type manifestHeader struct {
	Version       int           `json:"version"`
	Identity      string        `json:"identity"`
	Tip           Tip           `json:"tip"`
	Assembly      AssemblyState `json:"assembly"`
	Created       time.Time     `json:"created"`
	Complete      bool          `json:"complete"`
	GraphComplete bool          `json:"graph_complete"`
}

type Entry struct {
	Evidence Evidence `json:"evidence"`
	Snapshot Snapshot `json:"snapshot"`
	Blocked  bool     `json:"blocked"`
}

type manifest struct {
	*lockedDB
	header manifestHeader
	digest string
}

func validHash(id string) bool {
	h, err := chainhash.NewHashFromStr(id)
	return err == nil && len(id) == 64 && h.String() == id
}

func (m *manifest) checksum() (string, error) {
	h := sha256.New()
	b, err := json.Marshal(m.header)
	if err != nil {
		return "", err
	}
	_, _ = h.Write(b)
	for _, query := range []string{"SELECT id,json_array(CAST(entry AS TEXT),classification,blocked) FROM entries ORDER BY id", "SELECT child,parent FROM edges ORDER BY child,parent"} {
		rows, e := m.db.Query(query)
		if e != nil {
			return "", e
		}
		for rows.Next() {
			var a, b string
			if e = rows.Scan(&a, &b); e != nil {
				_ = rows.Close()
				return "", e
			}
			encoded, _ := json.Marshal([]string{a, b})
			_, _ = h.Write(encoded)
		}
		e = rows.Err()
		closeErr := rows.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openManifest(path string, auditOnly ...bool) (_ *manifest, err error) {
	db, err := openPrivateDB(path, false, true)
	if err != nil {
		return nil, err
	}
	m := &manifest{lockedDB: db}
	defer func() {
		if err != nil {
			_ = m.Close()
		}
	}()
	var header []byte
	if err = m.db.QueryRow("SELECT header,digest FROM manifest").Scan(&header, &m.digest); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(header, &m.header); err != nil {
		return nil, err
	}
	if m.header.Version != 1 || !m.header.Complete || (!m.header.GraphComplete && (len(auditOnly) == 0 || !auditOnly[0])) {
		return nil, failure("manifest is incomplete or unsupported")
	}
	digest, err := m.checksum()
	if err != nil {
		return nil, err
	}
	if digest != m.digest {
		return nil, failure("manifest integrity check failed")
	}
	return m, nil
}

func (m *manifest) summary() (Summary, error) {
	s := Summary{Stage: "discovered"}
	err := m.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(classification=? AND blocked=0),0),COALESCE(SUM(classification=?),0),COALESCE(SUM(classification=?),0),COALESCE(SUM(blocked),0) FROM entries", FullySpent, Live, Unknown).Scan(&s.Scanned, &s.FullySpent, &s.Live, &s.Unknown, &s.Blocked)
	return s, err
}

func (m *manifest) each(ctx context.Context, eligibleOnly bool, fn func(Entry) error) error {
	// Keyset pagination leaves the SQLite connection free while callbacks perform
	// journal writes and bounded network work; only one entry is resident at a time.
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		query := "SELECT id,entry FROM entries WHERE id>?"
		if eligibleOnly {
			query += " AND classification='fully-spent' AND blocked=0"
		}
		query += " ORDER BY id LIMIT 1"
		var id string
		var b []byte
		err := m.db.QueryRowContext(ctx, query, last).Scan(&id, &b)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var e Entry
		if err = json.Unmarshal(b, &e); err != nil {
			return err
		}
		if err = fn(e); err != nil {
			return err
		}
		last = id
	}
}

// ExportManifest emits a portable JSONL audit. The SQLite manifest remains the
// apply input, with an integrity seal covering entries and dependency edges.
func ExportManifest(ctx context.Context, path string, w io.Writer) error {
	m, err := openManifest(path, true)
	if err != nil {
		return err
	}
	defer m.Close() //nolint:errcheck // read-only
	enc := json.NewEncoder(w)
	if err = enc.Encode(map[string]any{"header": m.header, "digest": m.digest}); err != nil {
		return err
	}
	return m.each(ctx, false, func(e Entry) error { return enc.Encode(e) })
}

// Discover scans both the unmined store and a complete assembly snapshot without
// invoking any backend mutation. Unknown transactions remain visible in the audit.
func Discover(ctx context.Context, backend Backend, source Source, assembly Assembly, path string, progress func(Summary)) (result Summary, err error) {
	tip, err := source.Tip(ctx)
	if err != nil {
		return result, err
	}
	state, err := assembly.State(ctx)
	if err != nil {
		return result, err
	}
	if state.Tip != tip || !validHash(tip.Hash) || state.ProcessID == "" {
		return result, failure("local assembly and trusted source must agree on a valid tip")
	}
	db, err := openPrivateDB(path, true, false)
	if err != nil {
		return result, err
	}
	m := &manifest{lockedDB: db, header: manifestHeader{Version: 1, Identity: backend.Identity(), Tip: tip, Assembly: state, Created: time.Now().UTC(), GraphComplete: true}}
	defer func() { err = combineErrors(err, m.Close()) }()
	_, err = m.db.Exec("CREATE TABLE manifest(header BLOB NOT NULL,digest TEXT NOT NULL); CREATE TABLE entries(id TEXT PRIMARY KEY,entry BLOB NOT NULL DEFAULT '',classification TEXT NOT NULL DEFAULT '',blocked INTEGER NOT NULL DEFAULT 0); CREATE TABLE edges(child TEXT NOT NULL,parent TEXT NOT NULL,PRIMARY KEY(child,parent)); CREATE INDEX edge_parent ON edges(parent)")
	if err != nil {
		return result, err
	}
	emit := func(id string) error {
		if !validHash(id) {
			return failure("invalid discovered transaction identity")
		}
		_, e := m.db.ExecContext(ctx, "INSERT OR IGNORE INTO entries(id) VALUES (?)", id)
		if e == nil && progress != nil {
			result.Scanned++
			result.Stage = "enumerating"
			progress(result)
		}
		return e
	}
	if err = backend.Scan(ctx, emit); err != nil {
		return result, err
	}
	after, err := assembly.Transactions(ctx, emit)
	if err != nil {
		return result, err
	}
	if after.ProcessID != state.ProcessID || after.Tip != tip || after.ResetID != state.ResetID {
		return result, failure("assembly changed during discovery")
	}
	result.Scanned = 0
	result.Stage = "auditing"
	last := ""
	for {
		var id string
		e := m.db.QueryRowContext(ctx, "SELECT id FROM entries WHERE id>? ORDER BY id LIMIT 1", last).Scan(&id)
		if errors.Is(e, sql.ErrNoRows) {
			break
		}
		if e != nil {
			return result, e
		}
		inputs, e := backend.Inputs(ctx, id)
		if e != nil {
			m.header.GraphComplete = false
		}
		for _, parent := range inputs {
			if !validHash(parent) {
				return result, failure("invalid dependency for %s", id)
			}
			if _, e = m.db.ExecContext(ctx, "INSERT OR IGNORE INTO edges(child,parent) VALUES (?,?)", id, parent); e != nil {
				return result, e
			}
		}
		ev, e := source.Check(ctx, id, tip)
		if e != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			// Missing/pruned transaction history is an unresolved row, not the
			// end of a large audit. A failed or changed tip is still fatal.
			if tipErr := requireTip(ctx, source, tip); tipErr != nil {
				return result, tipErr
			}
			ev.Classification = Unknown
			ev.Reason = e.Error()
		}
		if ev.TxID != id || ev.Tip != tip {
			return result, failure("evidence identity/tip mismatch for %s", id)
		}
		entry := Entry{Evidence: ev}
		if ev.Classification == FullySpent {
			tx, parseErr := bt.NewTxFromString(ev.RawTx)
			if parseErr != nil || tx.TxID() != id || !validHash(ev.BlockHash) || ev.BlockHeight > tip.Height {
				return result, failure("invalid confirmed evidence for %s", id)
			}
			entry.Snapshot, e = backend.Snapshot(ctx, tx)
			if e != nil {
				entry.Evidence.Classification = Unknown
				entry.Evidence.Reason = "unsafe local snapshot: " + e.Error()
			}
		}
		if entry.Evidence.Classification != FullySpent && entry.Evidence.Classification != Live && entry.Evidence.Classification != Unknown {
			return result, failure("unsupported evidence classification")
		}
		b, e := json.Marshal(entry)
		if e != nil {
			return result, e
		}
		if _, e = m.db.ExecContext(ctx, "UPDATE entries SET entry=?,classification=? WHERE id=?", b, entry.Evidence.Classification, id); e != nil {
			return result, e
		}
		last = id
		result.Scanned++
		if progress != nil {
			progress(result)
		}
	}
	// Uncertain descendants or affected ancestors block the connected affected
	// component. Historical parents outside the scan are not treated as affected.
	_, err = m.db.ExecContext(ctx, `WITH RECURSIVE blocked(id) AS (
SELECT id FROM entries WHERE classification != 'fully-spent'
UNION SELECT CASE WHEN e.child=b.id THEN e.parent ELSE e.child END FROM edges e JOIN blocked b ON e.child=b.id OR e.parent=b.id JOIN entries n ON n.id=CASE WHEN e.child=b.id THEN e.parent ELSE e.child END
) UPDATE entries SET blocked=1 WHERE classification='fully-spent' AND id IN (SELECT id FROM blocked)`)
	if err != nil {
		return result, err
	}
	// Include block flags in the sealed entry as well as indexed columns.
	last = ""
	for {
		var id string
		var data []byte
		e := m.db.QueryRowContext(ctx, "SELECT id,entry FROM entries WHERE blocked=1 AND id>? ORDER BY id LIMIT 1", last).Scan(&id, &data)
		if errors.Is(e, sql.ErrNoRows) {
			break
		}
		if e != nil {
			return result, e
		}
		var entry Entry
		if e = json.Unmarshal(data, &entry); e != nil {
			return result, e
		}
		entry.Blocked = true
		data, e = json.Marshal(entry)
		if e != nil {
			return result, e
		}
		if _, e = m.db.ExecContext(ctx, "UPDATE entries SET entry=? WHERE id=?", data, id); e != nil {
			return result, e
		}
		last = id
	}
	end, err := source.Tip(ctx)
	if err != nil {
		return result, err
	}
	if end != tip {
		return result, failure("trusted tip changed during discovery")
	}
	endState, err := assembly.State(ctx)
	if err != nil {
		return result, err
	}
	if endState.ProcessID != state.ProcessID || endState.Tip != tip || endState.ResetID != state.ResetID {
		return result, failure("assembly changed during discovery")
	}
	m.header.Complete = true
	digest, err := m.checksum()
	if err != nil {
		return result, err
	}
	header, _ := json.Marshal(m.header)
	if _, err = m.db.ExecContext(ctx, "INSERT INTO manifest(header,digest) VALUES (?,?)", header, digest); err != nil {
		return result, err
	}
	result, err = m.summary()
	if err != nil {
		return result, err
	}
	if !m.header.GraphComplete {
		return result, failure("dependency enumeration incomplete: %w", ErrIncomplete)
	}
	if result.Unknown > 0 || result.Live > 0 || result.Blocked > 0 {
		return result, ErrIncomplete
	}
	return result, nil
}
