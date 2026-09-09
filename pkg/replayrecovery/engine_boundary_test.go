package replayrecovery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/stretchr/testify/require"
)

func TestApplyBoundaryResumesAfterMasterDeletion(t *testing.T) {
	backend, source, assembly, tx, manifestPath, journalPath := recoveryFixture(t)
	pageKey := tx.TxID() + ":page"
	backend.records[pageKey] = Record{Key: []byte(pageKey), Data: []byte("page before image"), Generation: 4}
	_, err := Discover(t.Context(), backend, source, assembly, manifestPath, nil)
	require.NoError(t, err)
	backend.failDeleteKey = pageKey
	_, err = Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.ErrorContains(t, err, "page deletion failed")
	require.NotContains(t, backend.records, tx.TxID(), "the interruption happens after master deletion")
	require.Contains(t, backend.records, pageKey)
	require.True(t, backend.HasMarker(backend.records["parent"], tx.TxID()))
	writes := backend.writes
	parent := backend.records["parent"]
	backend.failDeleteKey = ""
	result, err := Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Resume: true, Tip: source.tip})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Repaired)
	require.NotContains(t, backend.records, pageKey)
	require.Equal(t, writes+1, backend.writes, "resume deletes only the remaining page")
	require.True(t, backend.Equal(parent, backend.records["parent"]), "resume preserves the already verified marker generation")
}

func TestApplyBoundaryUnknownDescendantBlocksParent(t *testing.T) {
	backend, source, assembly, tx, manifestPath, journalPath := recoveryFixture(t)
	descendant := bt.NewTx()
	require.NoError(t, descendant.From(tx.TxID(), 0, tx.Outputs[0].LockingScript.String(), tx.Outputs[0].Satoshis))
	require.NoError(t, descendant.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 250))
	id := descendant.TxID()
	backend.txs[id] = descendant
	backend.records[id] = Record{Key: []byte(id), Data: []byte("unresolved descendant"), Generation: 3}
	backend.seeds = append(backend.seeds, id)
	assembly.ids = append(assembly.ids, id)
	summary, err := Discover(t.Context(), backend, source, assembly, manifestPath, nil)
	require.ErrorIs(t, err, ErrIncomplete)
	require.EqualValues(t, 1, summary.Unknown)
	require.EqualValues(t, 1, summary.Blocked)
	result, err := Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.ErrorIs(t, err, ErrIncomplete)
	require.Zero(t, result.Repaired)
	require.Zero(t, backend.writes, "unresolved descendants must prevent all writes to their component")
	require.Contains(t, backend.records, tx.TxID())
	require.Contains(t, backend.records, id)
	require.Equal(t, "spent:"+tx.TxID(), string(backend.records["parent"].Data))
}

func TestApplyBoundaryResealedInventoryCannotDeleteUnrelatedRecord(t *testing.T) {
	backend, source, assembly, tx, manifestPath, journalPath := recoveryFixture(t)
	victim := Record{Key: []byte("unrelated"), Data: []byte("must survive"), Generation: 9}
	backend.records["unrelated"] = victim
	_, err := Discover(t.Context(), backend, source, assembly, manifestPath, nil)
	require.NoError(t, err)
	// Recompute the integrity seal deliberately: a valid checksum cannot turn
	// operator-edited record keys into authority to delete unrelated data.
	db, err := openPrivateDB(manifestPath, false, false)
	require.NoError(t, err)
	m := &manifest{lockedDB: db}
	var header, data []byte
	require.NoError(t, m.db.QueryRow("SELECT header FROM manifest").Scan(&header))
	require.NoError(t, json.Unmarshal(header, &m.header))
	require.NoError(t, m.db.QueryRow("SELECT entry FROM entries WHERE id=?", tx.TxID()).Scan(&data))
	var entry Entry
	require.NoError(t, json.Unmarshal(data, &entry))
	entry.Snapshot.Records[0] = victim
	data, err = json.Marshal(entry)
	require.NoError(t, err)
	_, err = m.db.Exec("UPDATE entries SET entry=? WHERE id=?", data, tx.TxID())
	require.NoError(t, err)
	digest, err := m.checksum()
	require.NoError(t, err)
	_, err = m.db.Exec("UPDATE manifest SET digest=?", digest)
	require.NoError(t, err)
	require.NoError(t, m.Close())
	// Prove rejection comes from semantic authorization, not a stale checksum.
	accepted, err := openManifest(manifestPath)
	require.NoError(t, err)
	require.NoError(t, accepted.Close())
	_, err = Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.ErrorContains(t, err, "inventory does not match")
	require.Zero(t, backend.writes)
	require.True(t, backend.Equal(victim, backend.records["unrelated"]))
	require.Contains(t, backend.records, tx.TxID())
}

func TestVerifyBoundaryRequiresTipAfterReset(t *testing.T) {
	backend, source, assembly, _, manifestPath, journalPath := recoveryFixture(t)
	_, err := Discover(t.Context(), backend, source, assembly, manifestPath, nil)
	require.NoError(t, err)
	_, err = Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.NoError(t, err)
	assembly.ids = nil
	assembly.state.ProcessID = "restarted-after-maintenance"
	source.tip = Tip{Hash: "4444444444444444444444444444444444444444444444444444444444444444", Height: 201}
	assembly.state.Tip = source.tip
	result, err := Verify(t.Context(), backend, source, assembly, manifestPath, journalPath, true)
	require.ErrorIs(t, err, ErrPending, "a tip advance before this reset is not post-reset evidence")
	require.NotEqual(t, "verified", result.Stage)
	require.Equal(t, 1, assembly.resets)
	_, err = Verify(t.Context(), backend, source, assembly, manifestPath, journalPath, false)
	require.ErrorIs(t, err, ErrPending)
	source.tip = Tip{Hash: "5555555555555555555555555555555555555555555555555555555555555555", Height: 202}
	assembly.state.Tip = source.tip
	result, err = Verify(t.Context(), backend, source, assembly, manifestPath, journalPath, false)
	require.NoError(t, err)
	require.Equal(t, "verified", result.Stage)
	require.Equal(t, 1, assembly.resets)
}

func TestApplyBoundaryFiniteParentTTLNeverWrites(t *testing.T) {
	backend, source, assembly, tx, manifestPath, journalPath := recoveryFixture(t)
	parent := backend.records["parent"]
	parent.ExpiresAt = time.Now().Add(time.Hour).Unix()
	backend.records["parent"] = parent
	_, err := Discover(t.Context(), backend, source, assembly, manifestPath, nil)
	require.NoError(t, err)
	_, err = Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.Error(t, err, "finite parent TTL cannot guarantee marker survival across remote mutations")
	require.Zero(t, backend.writes)
	require.True(t, backend.Equal(parent, backend.records["parent"]))
	require.Contains(t, backend.records, tx.TxID())
}

type disappearingParentBackend struct {
	*recordBackend
	masterKey string
	deleted   []string
}

func (b *disappearingParentBackend) Delete(ctx context.Context, record Record) error {
	if err := b.recordBackend.Delete(ctx, record); err != nil {
		return err
	}
	b.deleted = append(b.deleted, string(record.Key))
	if string(record.Key) == b.masterKey {
		delete(b.records, "parent")
	}
	return nil
}

func TestApplyBoundaryParentDisappearsBetweenPages(t *testing.T) {
	base, source, assembly, tx, manifestPath, journalPath := recoveryFixture(t)
	backend := &disappearingParentBackend{recordBackend: base, masterKey: tx.TxID()}
	pageKey := tx.TxID() + ":page"
	backend.records[pageKey] = Record{Key: []byte(pageKey), Data: []byte("remaining page"), Generation: 4}
	_, err := Discover(t.Context(), backend, source, assembly, manifestPath, nil)
	require.NoError(t, err)
	_, err = Apply(t.Context(), backend, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.Error(t, err, "loss of a required marker must stop the next page deletion")
	require.Equal(t, []string{tx.TxID()}, backend.deleted)
	require.Contains(t, backend.records, pageKey)
}

type candidateBoundaryBackend struct{ *recordBackend }

func (b candidateBoundaryBackend) Transaction(_ context.Context, id string) (*bt.Tx, error) {
	return b.txs[id], nil
}

type candidateBoundarySource struct {
	*evidenceSource
	unavailable string
}

func (s candidateBoundarySource) Unspent(_ context.Context, id string, _ uint32, _ Tip) (bool, error) {
	return id != s.unavailable, nil
}

func TestCandidateBoundaryUnspendableOutputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		script    []byte
		height    uint32
		genesis   uint32
		spendable bool
	}{
		{"false-return after Genesis", []byte{0x00, 0x6a}, 200, 100, false},
		{"false-return before Genesis", []byte{0x00, 0x6a}, 50, 100, false},
		{"bare return before Genesis", []byte{0x6a}, 50, 100, false},
		{"bare return after Genesis", []byte{0x6a}, 200, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, source, _, _, _, journalPath := recoveryFixture(t)
			source.tip.Height = tc.height
			parent := bt.NewTx()
			require.NoError(t, parent.From("1111111111111111111111111111111111111111111111111111111111111111", 0, "51", 1000))
			parent.Outputs = []*bt.Output{{Satoshis: 500, LockingScript: bscript.NewFromBytes(tc.script)}}
			child := bt.NewTx()
			require.NoError(t, child.From(parent.TxID(), 0, parent.Outputs[0].LockingScript.String(), 500))
			require.NoError(t, child.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 250))
			base.txs[parent.TxID()] = parent
			base.txs[child.TxID()] = child
			backend := candidateBoundaryBackend{base}
			oracle := candidateBoundarySource{source, parent.TxID()}
			journal, err := openJournal(journalPath, false, "candidate-boundary", backend.Identity())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, journal.Close()) })
			_, err = journal.db.Exec("CREATE TABLE candidate_outputs(outpoint TEXT PRIMARY KEY); CREATE TABLE candidate_spends(outpoint TEXT PRIMARY KEY)")
			require.NoError(t, err)
			require.NoError(t, candidateAudit(t.Context(), journal, backend, oracle, source.tip, parent.TxID(), tc.genesis))
			err = candidateAudit(t.Context(), journal, backend, oracle, source.tip, child.TxID(), tc.genesis)
			if tc.spendable {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "unavailable chain output")
			}
		})
	}
}
