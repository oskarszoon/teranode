package rewindblockchain

import (
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// TestAllNotFound verifies the ChiR1 fix: the rewind delete tolerates an Unspend
// result only when EVERY error in the (possibly aggregated) chain is NotFound.
// A mixed aggregate — a legitimately-gone parent joined with a genuine
// StorageError — must surface, where the old isNotFound (any-link match) would
// have swallowed the real error.
func TestAllNotFound(t *testing.T) {
	require.False(t, allNotFound(nil), "nil is not a tolerated NotFound")

	// Single-error cases.
	require.True(t, allNotFound(errors.NewNotFoundError("output 1:2 not found")))
	require.True(t, allNotFound(errors.NewTxNotFoundError("tx gone")))
	require.False(t, allNotFound(errors.NewStorageError("device overload")))
	require.False(t, allNotFound(errors.NewProcessingError("some processing error")))

	// Aggregated (errors.Join) cases — fresh instances per call because Join
	// mutates its first arg's wrapped-error chain.
	require.True(t, allNotFound(errors.Join(
		errors.NewTxNotFoundError("a"),
		errors.NewNotFoundError("b"),
	)), "all-NotFound aggregate is tolerated")

	require.False(t, allNotFound(errors.Join(
		errors.NewTxNotFoundError("a"),
		errors.NewStorageError("boom"),
	)), "mixed aggregate (NotFound first) must NOT be tolerated")

	require.False(t, allNotFound(errors.Join(
		errors.NewStorageError("boom"),
		errors.NewTxNotFoundError("a"),
	)), "mixed aggregate (StorageError first) must NOT be tolerated")

	// Contrast: the old isNotFound wrongly tolerates the mixed case — this is
	// exactly the swallowed-error bug allNotFound fixes.
	require.True(t, isNotFound(errors.Join(
		errors.NewTxNotFoundError("a"),
		errors.NewStorageError("boom"),
	)), "isNotFound matches any link — demonstrates why allNotFound is needed")
}
