package replayrecovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type changedAuditAssembly struct {
	*assemblyFixture
	change string
}

func (a changedAuditAssembly) Transactions(ctx context.Context, visit func(string) error) (AssemblyState, error) {
	s, err := a.assemblyFixture.Transactions(ctx, visit)
	if a.change == "assembly reset" {
		s.ResetID++
	}
	return s, err
}
func (a changedAuditAssembly) Candidate(ctx context.Context, visit func(string) error) (AssemblyState, error) {
	s, err := a.assemblyFixture.Candidate(ctx, visit)
	if a.change == "missing mining job" {
		s.CandidateID = ""
	}
	if a.change == "candidate process" {
		s.ProcessID = "another-instance"
	}
	return s, err
}
func (a changedAuditAssembly) Reset(ctx context.Context) (AssemblyState, error) {
	s, err := a.assemblyFixture.Reset(ctx)
	if a.change == "reset acknowledgement only" {
		s.ResetID--
	}
	return s, err
}

func TestVerifyRejectsPostRepairCorruptionAndStaleServiceEvidence(t *testing.T) {
	for _, mode := range []string{"recreated child", "missing parent", "lost marker", "live output", "assembly contains replay", "invalid assembly identity", "assembly reset", "missing mining job", "candidate process", "reset acknowledgement only"} {
		t.Run(mode, func(t *testing.T) {
			base, source, assembly, tx, manifest, journal := recoveryFixture(t)
			before := base.records[tx.TxID()]
			_, err := Discover(t.Context(), base, source, assembly, manifest, nil)
			require.NoError(t, err)
			_, err = Apply(t.Context(), base, source, manifest, journal, ApplyOptions{Maintenance: true, Tip: source.tip})
			require.NoError(t, err)
			assembly.ids = nil
			assembly.state.ProcessID = "after-maintenance"
			switch mode {
			case "recreated child":
				base.records[tx.TxID()] = before
			case "missing parent":
				delete(base.records, "parent")
			case "lost marker":
				r := base.records["parent"]
				r.Data = []byte("spent:" + tx.TxID())
				base.records["parent"] = r
			case "live output":
				ev := source.evidence[tx.TxID()]
				ev.Classification = Live
				source.evidence[tx.TxID()] = ev
			case "assembly contains replay":
				assembly.ids = []string{tx.TxID()}
			case "invalid assembly identity":
				assembly.ids = []string{"truncated-txid"}
			}
			writes := base.writes
			result, err := Verify(t.Context(), base, source, changedAuditAssembly{assembly, mode}, manifest, journal, true)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrPending, "corruption must not be mistaken for a healthy recovery merely awaiting a tip")
			require.NotEqual(t, "verified", result.Stage)
			require.Equal(t, writes, base.writes, "verification never repairs corruption implicitly")
		})
	}
}

func TestVerifyRejectsCorruptResetCheckpoints(t *testing.T) {
	for _, checkpoint := range []string{`not-json`, `{"process_id":"before-repair","reset_id":1}`, `{"process_id":"after-maintenance","reset_id":0}`} {
		t.Run(checkpoint, func(t *testing.T) {
			base, source, assembly, _, manifestPath, journalPath := recoveryFixture(t)
			_, err := Discover(t.Context(), base, source, assembly, manifestPath, nil)
			require.NoError(t, err)
			_, err = Apply(t.Context(), base, source, manifestPath, journalPath, ApplyOptions{Maintenance: true, Tip: source.tip})
			require.NoError(t, err)
			assembly.ids = nil
			assembly.state.ProcessID = "after-maintenance"
			_, err = Verify(t.Context(), base, source, assembly, manifestPath, journalPath, true)
			require.ErrorIs(t, err, ErrPending)
			m, err := openManifest(manifestPath)
			require.NoError(t, err)
			j, err := openJournal(journalPath, true, m.digest, base.Identity())
			require.NoError(t, err)
			require.NoError(t, j.put("reset-completed", []byte(checkpoint)))
			require.NoError(t, j.Close())
			require.NoError(t, m.Close())
			_, err = Verify(t.Context(), base, source, assembly, manifestPath, journalPath, false)
			require.ErrorContains(t, err, "invalid reset completion checkpoint")
			require.Equal(t, 1, assembly.resets, "corrupt recorded outcomes cannot silently trigger another reset")
		})
	}
}
