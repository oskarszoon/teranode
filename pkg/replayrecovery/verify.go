package replayrecovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

type localTransactionReader interface {
	Transaction(context.Context, string) (*bt.Tx, error)
}
type outputSource interface {
	Unspent(context.Context, string, uint32, Tip) (bool, error)
}

type parentVerifier interface {
	VerifyParent(context.Context, Parent) error
}

func verifyStore(ctx context.Context, m *manifest, j *journal, backend Backend, source Source, tip Tip) error {
	return m.each(ctx, true, func(e Entry) error {
		for _, r := range e.Snapshot.Records {
			current, err := backend.Read(ctx, r.Key)
			if err != nil {
				return err
			}
			if current != nil {
				return failure("repaired transaction record reappeared: %s", e.Evidence.TxID)
			}
		}
		for _, p := range e.Snapshot.Parents {
			current, err := backend.Read(ctx, p.Record.Key)
			if err != nil {
				return err
			}
			if current == nil {
				step, err := j.step(fmt.Sprintf("delete/%x", p.Record.Key))
				if err != nil {
					return err
				}
				if step == nil || !step.Done {
					return failure("required surviving parent is missing: %s", e.Evidence.TxID)
				}
			} else {
				if !backend.HasMarker(*current, p.Child) {
					return failure("required replay marker disappeared: %s", p.Child)
				}
				verifier, ok := backend.(parentVerifier)
				if !ok {
					return failure("backend cannot verify preserved parent spends: %w", ErrIncomplete)
				}
				if err := verifier.VerifyParent(ctx, p); err != nil {
					return err
				}
			}
		}
		ev, err := source.Check(ctx, e.Evidence.TxID, tip)
		if err != nil {
			return err
		}
		if ev.TxID != e.Evidence.TxID || ev.RawTx != e.Evidence.RawTx || ev.Tip != tip || ev.Classification != FullySpent {
			return failure("repaired transaction no longer proven fully spent: %s", e.Evidence.TxID)
		}
		return nil
	})
}

// candidateAudit verifies UTXO availability and duplicate spends independently
// of local spent metadata. It is deliberately not a script/consensus validator.
func candidateAudit(ctx context.Context, j *journal, backend Backend, source Source, tip Tip, id string, genesisHeight ...uint32) error {
	reader, ok := backend.(localTransactionReader)
	if !ok {
		return failure("backend cannot supply candidate transaction bytes: %w", ErrIncomplete)
	}
	oracle, ok := source.(outputSource)
	if !ok {
		return failure("source cannot audit candidate inputs: %w", ErrIncomplete)
	}
	tx, err := reader.Transaction(ctx, id)
	if err != nil {
		return err
	}
	if tx == nil || tx.TxID() != id {
		return failure("candidate transaction identity mismatch")
	}
	for _, in := range tx.Inputs {
		parent := in.PreviousTxIDChainHash().String()
		key := fmt.Sprintf("%s:%d", parent, in.PreviousTxOutIndex)
		if _, err = j.db.ExecContext(ctx, "INSERT INTO candidate_spends(outpoint) VALUES (?)", key); err != nil {
			return failure("candidate duplicate spend %s: %w", key, err)
		}
		var exists int
		err = j.db.QueryRowContext(ctx, "SELECT 1 FROM candidate_outputs WHERE outpoint=?", key).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			live, e := oracle.Unspent(ctx, parent, in.PreviousTxOutIndex, tip)
			if e != nil {
				return e
			}
			if !live {
				return failure("candidate spends unavailable chain output %s", key)
			}
		} else if err != nil {
			return err
		}
	}
	var activation uint32
	if len(genesisHeight) > 0 {
		activation = genesisHeight[0]
	}
	if tip.Height == ^uint32(0) {
		return failure("candidate height overflow")
	}
	for n, output := range tx.Outputs {
		if output == nil || !utxo.ShouldStoreOutputAsUTXO(output, tip.Height+1, activation) {
			continue
		}
		key := fmt.Sprintf("%s:%d", id, n)
		if _, err = j.db.ExecContext(ctx, "INSERT INTO candidate_outputs(outpoint) VALUES (?)", key); err != nil {
			return failure("duplicate candidate transaction: %w", err)
		}
	}
	return nil
}

// Verify is deliberately repeatable. The first post-maintenance invocation uses
// reset=true and records actual reset completion. Later invocations require an
// observed process restart and a subsequent tip transition before success.
func Verify(ctx context.Context, backend Backend, source Source, assembly Assembly, manifestPath, journalPath string, reset bool, genesisHeight ...uint32) (result Summary, err error) {
	m, err := openManifest(manifestPath)
	if err != nil {
		return result, err
	}
	defer func() { err = combineErrors(err, m.Close()) }()
	if backend.Identity() != m.header.Identity {
		return result, failure("manifest belongs to another backend")
	}
	j, err := openJournal(journalPath, true, m.digest, backend.Identity())
	if err != nil {
		return result, err
	}
	defer func() { err = combineErrors(err, j.Close()) }()
	applied, err := j.get("applied")
	if err != nil {
		return result, err
	}
	if applied == nil {
		return result, failure("apply has not completed")
	}
	var appliedTip Tip
	if err = json.Unmarshal(applied, &appliedTip); err != nil {
		return result, err
	}
	result, err = m.summary()
	if err != nil {
		return result, err
	}
	result.Stage = "verification-pending"
	state, err := assembly.State(ctx)
	if err != nil {
		return result, err
	}
	if state.ProcessID == "" || state.ProcessID == m.header.Assembly.ProcessID {
		return result, failure("restart since maintenance has not been observed: %w", ErrPending)
	}
	if err = requireTip(ctx, source, state.Tip); err != nil {
		return result, err
	}
	checkpoint, err := j.get("reset-completed")
	if err != nil {
		return result, err
	}
	if checkpoint == nil {
		if !reset {
			return result, failure("ordinary reset must complete before verification: %w", ErrPending)
		}
		after, e := assembly.Reset(ctx)
		if e != nil {
			return result, e
		}
		if after.ProcessID != state.ProcessID || after.Tip != state.Tip || after.ResetID <= state.ResetID {
			return result, failure("reset did not return a new completion on the same process and tip")
		}
		state = after
		checkpoint, _ = json.Marshal(after)
		if err = j.put("reset-completed", checkpoint); err != nil {
			return result, err
		}
	}
	var completed AssemblyState
	if err = json.Unmarshal(checkpoint, &completed); err != nil {
		return result, failure("invalid reset completion checkpoint: %w", err)
	}
	if completed.ProcessID == "" || completed.ResetID == 0 || !validHash(completed.Tip.Hash) || completed.ProcessID == m.header.Assembly.ProcessID {
		return result, failure("invalid reset completion checkpoint")
	}
	if err = verifyStore(ctx, m, j, backend, source, state.Tip); err != nil {
		return result, err
	}
	checkAbsent := func(id string) error {
		if !validHash(id) {
			return failure("invalid assembly transaction identity")
		}
		var present int
		e := j.db.QueryRowContext(ctx, "SELECT 1 FROM work WHERE id=? AND done=1", id).Scan(&present)
		if e == nil {
			return failure("repaired transaction present in fresh assembly/candidate: %s", id)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		return nil
	}
	snapshot, err := assembly.Transactions(ctx, checkAbsent)
	if err != nil {
		return result, err
	}
	if snapshot.ProcessID != state.ProcessID || snapshot.Tip != state.Tip || snapshot.ResetID != state.ResetID {
		return result, failure("assembly changed during verification")
	}
	_, err = j.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS candidate_outputs(outpoint TEXT PRIMARY KEY); CREATE TABLE IF NOT EXISTS candidate_spends(outpoint TEXT PRIMARY KEY); DELETE FROM candidate_outputs; DELETE FROM candidate_spends")
	if err != nil {
		return result, err
	}
	candidate, err := assembly.Candidate(ctx, func(id string) error {
		if err := checkAbsent(id); err != nil {
			return err
		}
		return candidateAudit(ctx, j, backend, source, state.Tip, id, genesisHeight...)
	})
	if err != nil {
		return result, err
	}
	if candidate.CandidateID == "" || candidate.ProcessID != state.ProcessID || candidate.Tip != state.Tip || candidate.ResetID != state.ResetID {
		return result, failure("candidate changed during verification")
	}
	if err = requireTip(ctx, source, state.Tip); err != nil {
		return result, err
	}
	b, _ := json.Marshal(candidate)
	if err = j.put("last-verified-candidate", b); err != nil {
		return result, err
	}
	if state.Tip.Height <= appliedTip.Height || state.Tip.Hash == appliedTip.Hash || state.Tip.Height <= completed.Tip.Height || state.Tip.Hash == completed.Tip.Hash {
		return result, failure("waiting for a subsequent chain-tip transition: %w", ErrPending)
	}
	if result.Unknown > 0 || result.Live > 0 || result.Blocked > 0 {
		return result, ErrIncomplete
	}
	result.Stage = "verified"
	if err = j.put("verified", b); err != nil {
		return result, err
	}
	return result, nil
}
