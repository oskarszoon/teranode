package replayrecovery

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These faults model outcomes at the remote mutation boundary. Each assertion
// checks which records survive, not merely that a backend error was forwarded.
type mutationFaultBackend struct {
	*recordBackend
	ambiguousDelete  bool
	ignoredDelete    bool
	wrongMarkerReply bool
}

func (b *mutationFaultBackend) Delete(ctx context.Context, r Record) error {
	if b.ignoredDelete {
		return nil
	}
	if err := b.recordBackend.Delete(ctx, r); err != nil {
		return err
	}
	if b.ambiguousDelete {
		return failure("delete response lost")
	}
	return nil
}

func (b *mutationFaultBackend) Mark(ctx context.Context, r Record, id string) (Record, error) {
	after, err := b.recordBackend.Mark(ctx, r, id)
	if err == nil && b.wrongMarkerReply {
		after.Generation++
	}
	return after, err
}

func TestApplyLostDeleteResponseResumesWithoutAnotherMutation(t *testing.T) {
	base, source, assembly, tx, manifest, journal := recoveryFixture(t)
	backend := &mutationFaultBackend{recordBackend: base, ambiguousDelete: true}
	_, err := Discover(t.Context(), backend, source, assembly, manifest, nil)
	require.NoError(t, err)
	_, err = Apply(t.Context(), backend, source, manifest, journal, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.ErrorContains(t, err, "delete response lost")
	require.NotContains(t, base.records, tx.TxID())
	require.True(t, base.HasMarker(base.records["parent"], tx.TxID()))
	writes := base.writes
	backend.ambiguousDelete = false
	result, err := Apply(t.Context(), backend, source, manifest, journal, ApplyOptions{Maintenance: true, Resume: true, Tip: source.tip})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Repaired)
	require.Equal(t, writes, base.writes, "recorded intent plus authoritative absence must not repeat deletion")
}

func TestApplyRejectsFalseMutationAcknowledgements(t *testing.T) {
	for _, mode := range []string{"ignored delete", "wrong marker generation"} {
		t.Run(mode, func(t *testing.T) {
			base, source, assembly, tx, manifest, journal := recoveryFixture(t)
			backend := &mutationFaultBackend{recordBackend: base, ignoredDelete: mode == "ignored delete", wrongMarkerReply: mode == "wrong marker generation"}
			_, err := Discover(t.Context(), backend, source, assembly, manifest, nil)
			require.NoError(t, err)
			_, err = Apply(t.Context(), backend, source, manifest, journal, ApplyOptions{Maintenance: true, Tip: source.tip})
			require.Error(t, err)
			require.Contains(t, base.records, tx.TxID(), "a successful RPC response is insufficient without matching readback")
			backend.ignoredDelete = false
			backend.wrongMarkerReply = false
			_, err = Apply(t.Context(), backend, source, manifest, journal, ApplyOptions{Maintenance: true, Resume: true, Tip: source.tip})
			require.NoError(t, err)
			require.NotContains(t, base.records, tx.TxID())
		})
	}
}

func TestResumeRefusesUnrelatedParentChange(t *testing.T) {
	base, source, assembly, tx, manifest, journal := recoveryFixture(t)
	_, err := Discover(t.Context(), base, source, assembly, manifest, nil)
	require.NoError(t, err)
	base.failAfterMark = true
	_, err = Apply(t.Context(), base, source, manifest, journal, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.Error(t, err)
	r := base.records["parent"]
	r.Generation++
	base.records["parent"] = r
	base.failAfterMark = false
	writes := base.writes
	_, err = Apply(t.Context(), base, source, manifest, journal, ApplyOptions{Maintenance: true, Resume: true, Tip: source.tip})
	require.ErrorContains(t, err, "parent generation or before-state changed")
	require.Equal(t, writes, base.writes)
	require.Contains(t, base.records, tx.TxID(), "an existing marker cannot authorize a divergent parent")
}

func TestResumeRejectsRecreatedMasterBeforeDeletingRemainingPage(t *testing.T) {
	base, source, assembly, tx, manifest, journal := recoveryFixture(t)
	key := tx.TxID() + ":page"
	master := base.records[tx.TxID()]
	base.records[key] = Record{Key: []byte(key), Data: []byte("page"), Generation: 3}
	_, err := Discover(t.Context(), base, source, assembly, manifest, nil)
	require.NoError(t, err)
	base.failDeleteKey = key
	_, err = Apply(t.Context(), base, source, manifest, journal, ApplyOptions{Maintenance: true, Tip: source.tip})
	require.Error(t, err)
	base.records[tx.TxID()] = master
	base.failDeleteKey = ""
	writes := base.writes
	_, err = Apply(t.Context(), base, source, manifest, journal, ApplyOptions{Maintenance: true, Resume: true, Tip: source.tip})
	require.ErrorContains(t, err, "deleted record reappeared")
	require.Equal(t, writes, base.writes)
	require.Contains(t, base.records, key)
}

func TestApplyRevalidatesDiscoveryCoverageAndCanonicalEvidence(t *testing.T) {
	for _, mode := range []string{"new unmined record", "changed dependency", "live output", "changed tip"} {
		t.Run(mode, func(t *testing.T) {
			base, source, assembly, tx, manifest, journal := recoveryFixture(t)
			_, err := Discover(t.Context(), base, source, assembly, manifest, nil)
			require.NoError(t, err)
			tip := source.tip
			switch mode {
			case "new unmined record":
				base.seeds = append(base.seeds, strings.Repeat("9", 64))
			case "changed dependency":
				delete(base.txs, tx.TxID())
			case "live output":
				ev := source.evidence[tx.TxID()]
				ev.Classification = Live
				source.evidence[tx.TxID()] = ev
			case "changed tip":
				source.tip.Hash = strings.Repeat("9", 64)
			}
			_, err = Apply(t.Context(), base, source, manifest, journal, ApplyOptions{Maintenance: true, Tip: tip})
			require.Error(t, err)
			require.Zero(t, base.writes)
			require.Contains(t, base.records, tx.TxID())
		})
	}
}
