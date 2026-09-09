package replayrecovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/stretchr/testify/require"
)

type recordBackend struct {
	records       map[string]Record
	txs           map[string]*bt.Tx
	seeds         []string
	writes        int
	failMark      bool
	failAfterMark bool
	failDeleteKey string
}

func (b *recordBackend) Identity() string { return "test-store" }
func (b *recordBackend) Scan(ctx context.Context, emit func(string) error) error {
	for _, id := range b.seeds {
		if err := emit(id); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (b *recordBackend) Inputs(_ context.Context, id string) ([]string, error) {
	tx := b.txs[id]
	if tx == nil {
		return nil, nil
	}
	out := []string{}
	for _, in := range tx.Inputs {
		out = append(out, in.PreviousTxIDChainHash().String())
	}
	return out, nil
}
func (b *recordBackend) Read(_ context.Context, key []byte) (*Record, error) {
	r, ok := b.records[string(key)]
	if !ok {
		return nil, nil
	}
	return &r, nil
}
func (b *recordBackend) Equal(a, c Record) bool {
	return a.Generation == c.Generation && string(a.Data) == string(c.Data) && a.ExpiresAt == c.ExpiresAt && string(a.Key) == string(c.Key)
}
func (b *recordBackend) HasMarker(r Record, child string) bool {
	return string(r.Data) == "spent:"+child+":marked"
}
func (b *recordBackend) MatchesMarked(a, c Record, child string) bool {
	return c.Generation == a.Generation+1 && c.ExpiresAt == a.ExpiresAt && string(a.Data) == "spent:"+child && b.HasMarker(c, child)
}
func (b *recordBackend) Snapshot(_ context.Context, tx *bt.Tx) (Snapshot, error) {
	id := tx.TxID()
	child, ok := b.records[id]
	if !ok {
		return Snapshot{}, failure("missing child")
	}
	parent := b.records["parent"]
	records := []Record{child}
	if page, ok := b.records[id+":page"]; ok {
		records = append(records, page)
	}
	return Snapshot{TxID: id, Records: records, Parents: []Parent{{Record: parent, Child: id}}}, nil
}
func (b *recordBackend) Mark(_ context.Context, r Record, child string) (Record, error) {
	b.writes++
	if b.failMark {
		return Record{}, failure("marker failed")
	}
	cur, ok := b.records[string(r.Key)]
	if !ok || !b.Equal(r, cur) {
		return Record{}, failure("generation conflict")
	}
	r.Generation++
	r.Data = []byte("spent:" + child + ":marked")
	b.records[string(r.Key)] = r
	if b.failAfterMark {
		return Record{}, failure("ambiguous marker response")
	}
	return r, nil
}
func (b *recordBackend) Delete(_ context.Context, r Record) error {
	b.writes++
	if string(r.Key) == b.failDeleteKey {
		return failure("page deletion failed")
	}
	cur, ok := b.records[string(r.Key)]
	if !ok || !b.Equal(r, cur) {
		return failure("generation conflict")
	}
	delete(b.records, string(r.Key))
	return nil
}

func (b *recordBackend) VerifyParent(_ context.Context, p Parent) error {
	r, ok := b.records[string(p.Record.Key)]
	if !ok || !b.HasMarker(r, p.Child) {
		return failure("parent spend changed")
	}
	return nil
}

type evidenceSource struct {
	tip      Tip
	evidence map[string]Evidence
}

func (s *evidenceSource) Tip(context.Context) (Tip, error) { return s.tip, nil }
func (s *evidenceSource) Check(_ context.Context, id string, tip Tip) (Evidence, error) {
	e, ok := s.evidence[id]
	if !ok {
		return Evidence{TxID: id, Tip: tip, Classification: Unknown, Reason: "not found"}, nil
	}
	e.Tip = tip
	return e, nil
}

type assemblyFixture struct {
	state  AssemblyState
	ids    []string
	resets int
}

func (a *assemblyFixture) State(context.Context) (AssemblyState, error) { return a.state, nil }
func (a *assemblyFixture) Transactions(_ context.Context, emit func(string) error) (AssemblyState, error) {
	for _, id := range a.ids {
		if err := emit(id); err != nil {
			return AssemblyState{}, err
		}
	}
	return a.state, nil
}
func (a *assemblyFixture) Candidate(ctx context.Context, emit func(string) error) (AssemblyState, error) {
	s, err := a.Transactions(ctx, emit)
	s.CandidateID = "fresh-candidate"
	return s, err
}
func (a *assemblyFixture) Reset(context.Context) (AssemblyState, error) {
	a.resets++
	a.state.ResetID++
	return a.state, nil
}

func recoveryFixture(t *testing.T) (*recordBackend, *evidenceSource, *assemblyFixture, *bt.Tx, string, string) {
	t.Helper()
	tx := bt.NewTx()
	require.NoError(t, tx.From("1111111111111111111111111111111111111111111111111111111111111111", 0, "51", 1000))
	require.NoError(t, tx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 500))
	id := tx.TxID()
	b := &recordBackend{records: map[string]Record{"parent": {Key: []byte("parent"), Data: []byte("spent:" + id), Generation: 7}, id: {Key: []byte(id), Data: []byte("unmined"), Generation: 2}}, txs: map[string]*bt.Tx{id: tx}, seeds: []string{id}}
	tip := Tip{Hash: "2222222222222222222222222222222222222222222222222222222222222222", Height: 200}
	s := &evidenceSource{tip: tip, evidence: map[string]Evidence{id: {TxID: id, RawTx: tx.String(), BlockHash: "3333333333333333333333333333333333333333333333333333333333333333", BlockHeight: 100, Classification: FullySpent}}}
	a := &assemblyFixture{state: AssemblyState{ProcessID: "before-repair", Tip: tip}, ids: []string{id}}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	return b, s, a, tx, filepath.Join(dir, "manifest.db"), filepath.Join(dir, "journal.db")
}

func TestDiscoveryDefaultNeverWrites(t *testing.T) {
	b, s, a, _, manifest, _ := recoveryFixture(t)
	summary, err := Discover(t.Context(), b, s, a, manifest, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.FullySpent)
	require.Zero(t, b.writes)
}

func TestApplyMarkerFailureNeverDeletesChild(t *testing.T) {
	b, s, a, tx, manifest, journal := recoveryFixture(t)
	_, err := Discover(t.Context(), b, s, a, manifest, nil)
	require.NoError(t, err)
	b.failMark = true
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Maintenance: true, Tip: s.tip})
	require.Error(t, err)
	require.Contains(t, b.records, tx.TxID())
	require.Equal(t, "spent:"+tx.TxID(), string(b.records["parent"].Data))
	b.failMark = false
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Maintenance: true, Resume: true, Tip: s.tip})
	require.NoError(t, err)
	require.NotContains(t, b.records, tx.TxID())
	require.True(t, b.HasMarker(b.records["parent"], tx.TxID()))
}

func TestApplyResumesAmbiguousVerifiedMarker(t *testing.T) {
	b, s, a, tx, manifest, journal := recoveryFixture(t)
	_, err := Discover(t.Context(), b, s, a, manifest, nil)
	require.NoError(t, err)
	b.failAfterMark = true
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Maintenance: true, Tip: s.tip})
	require.Error(t, err)
	require.Contains(t, b.records, tx.TxID())
	b.failAfterMark = false
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Maintenance: true, Resume: true, Tip: s.tip})
	require.NoError(t, err)
	require.NotContains(t, b.records, tx.TxID())
}

func TestApplyRejectsGenerationConflictAndMissingMaintenance(t *testing.T) {
	b, s, a, tx, manifest, journal := recoveryFixture(t)
	_, err := Discover(t.Context(), b, s, a, manifest, nil)
	require.NoError(t, err)
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Tip: s.tip})
	require.Error(t, err)
	require.Zero(t, b.writes)
	r := b.records[tx.TxID()]
	r.Generation++
	b.records[tx.TxID()] = r
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Maintenance: true, Tip: s.tip})
	require.Error(t, err)
	require.Contains(t, b.records, tx.TxID())
	require.Zero(t, b.writes)
}

func TestVerifyRequiresRestartAndLaterTip(t *testing.T) {
	b, s, a, _, manifest, journal := recoveryFixture(t)
	_, err := Discover(t.Context(), b, s, a, manifest, nil)
	require.NoError(t, err)
	_, err = Apply(t.Context(), b, s, manifest, journal, ApplyOptions{Maintenance: true, Tip: s.tip})
	require.NoError(t, err)
	a.ids = nil
	_, err = Verify(t.Context(), b, s, a, manifest, journal, true)
	require.ErrorIs(t, err, ErrPending)
	a.state.ProcessID = "after-repair"
	_, err = Verify(t.Context(), b, s, a, manifest, journal, true)
	require.ErrorIs(t, err, ErrPending)
	s.tip = Tip{Hash: "4444444444444444444444444444444444444444444444444444444444444444", Height: 201}
	a.state.Tip = s.tip
	_, err = Verify(t.Context(), b, s, a, manifest, journal, false)
	require.NoError(t, err)
}
