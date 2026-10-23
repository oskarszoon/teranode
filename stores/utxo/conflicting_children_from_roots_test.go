package utxo

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The roots of a block's conflicting transactions overlap: in a linear
// conflicting chain every member's cone contains every later member. One walk per
// root re-reads the shared part once per root (issue 2037), so the multi-root
// walk must read each reachable transaction once, however many roots reach it.
func TestGetConflictingChildrenFromRoots_SharesVisitedAcrossRoots(t *testing.T) {
	rootA := createTestHash("roots-a")
	rootB := createTestHash("roots-b")
	shared := createTestHash("roots-shared")
	tip := createTestHash("roots-tip")

	// rootA -> rootB -> shared -> tip, and rootA -> shared directly
	mockStore := &MockUtxostore{}
	mockStore.On("Get", mock.Anything, &rootA, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &rootB}, {TxID: &shared}}}, nil).Once()
	mockStore.On("Get", mock.Anything, &rootB, mock.Anything).
		Return(&meta.Data{ConflictingChildren: []chainhash.Hash{shared}}, nil).Once()
	mockStore.On("Get", mock.Anything, &shared, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &tip}}}, nil).Once()
	mockStore.On("Get", mock.Anything, &tip, mock.Anything).
		Return(&meta.Data{}, nil).Once()

	visited := make(map[chainhash.Hash]struct{})

	children, err := GetConflictingChildrenFromRoots(context.Background(), mockStore, []chainhash.Hash{rootA, rootB}, visited, 0)
	require.NoError(t, err)

	// the roots are not their own descendants, even when one reaches the other
	require.ElementsMatch(t, []chainhash.Hash{shared, tip}, children)
	require.Len(t, visited, 4)

	mockStore.AssertExpectations(t)
	mockStore.AssertNumberOfCalls(t, "Get", 4)
}

// The budget bounds the union, not each root's cone: a caller that walks many
// roots in one or several calls on the same visited set gets one budget for all
// of them. Each cone here fits the budget on its own; together they do not.
func TestGetConflictingChildrenFromRoots_BudgetCoversUnion(t *testing.T) {
	rootA := createTestHash("budget-union-a")
	rootB := createTestHash("budget-union-b")
	childA := createTestHash("budget-union-child-a")
	childB := createTestHash("budget-union-child-b")

	newStore := func() *MockUtxostore {
		mockStore := &MockUtxostore{}
		mockStore.On("Get", mock.Anything, &rootA, mock.Anything).
			Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &childA}}}, nil).Maybe()
		mockStore.On("Get", mock.Anything, &rootB, mock.Anything).
			Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &childB}}}, nil).Maybe()
		mockStore.On("Get", mock.Anything, &childA, mock.Anything).Return(&meta.Data{}, nil).Maybe()
		mockStore.On("Get", mock.Anything, &childB, mock.Anything).Return(&meta.Data{}, nil).Maybe()

		return mockStore
	}

	t.Run("each cone alone fits", func(t *testing.T) {
		for _, root := range []chainhash.Hash{rootA, rootB} {
			children, err := GetConflictingChildren(context.Background(), newStore(), root, 3)
			require.NoError(t, err)
			require.Len(t, children, 1)
		}
	})

	t.Run("union of one call exceeds", func(t *testing.T) {
		_, err := GetConflictingChildrenFromRoots(context.Background(), newStore(), []chainhash.Hash{rootA, rootB}, make(map[chainhash.Hash]struct{}), 3)
		require.ErrorIs(t, err, errors.ErrUtxoWalkLimitExceeded)
		require.NotErrorIs(t, err, errors.ErrTxInvalid)
	})

	t.Run("union across calls on one visited set exceeds", func(t *testing.T) {
		mockStore := newStore()
		visited := make(map[chainhash.Hash]struct{})

		_, err := GetConflictingChildrenFromRoots(context.Background(), mockStore, []chainhash.Hash{rootA}, visited, 3)
		require.NoError(t, err)

		_, err = GetConflictingChildrenFromRoots(context.Background(), mockStore, []chainhash.Hash{rootB}, visited, 3)
		require.ErrorIs(t, err, errors.ErrUtxoWalkLimitExceeded)
	})

	t.Run("already visited nodes are not read again or returned", func(t *testing.T) {
		mockStore := newStore()
		visited := map[chainhash.Hash]struct{}{rootA: {}, childA: {}}

		children, err := GetConflictingChildrenFromRoots(context.Background(), mockStore, []chainhash.Hash{rootA, rootB}, visited, 0)
		require.NoError(t, err)
		require.Equal(t, []chainhash.Hash{childB}, children)

		mockStore.AssertNotCalled(t, "Get", mock.Anything, &rootA, mock.Anything)
		mockStore.AssertNotCalled(t, "Get", mock.Anything, &childA, mock.Anything)
	})
}

// A transaction the store no longer holds says nothing about what spent it, but
// it must not hide the rest of the cone: it is a leaf, and its siblings and their
// descendants are still walked. Both not-found shapes are covered: the SQL store
// returns ErrTxNotFound, aerospike returns a nil record.
func TestGetConflictingChildrenFromRoots_MissingRecordIsLeaf(t *testing.T) {
	root := createTestHash("missing-root")
	goneErr := createTestHash("missing-gone-err")
	goneNil := createTestHash("missing-gone-nil")
	sibling := createTestHash("missing-sibling")
	grandchild := createTestHash("missing-grandchild")

	mockStore := &MockUtxostore{}
	mockStore.On("Get", mock.Anything, &root, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &goneErr}, {TxID: &goneNil}, {TxID: &sibling}}}, nil)
	mockStore.On("Get", mock.Anything, &goneErr, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("gone"))
	mockStore.On("Get", mock.Anything, &goneNil, mock.Anything).
		Return(nil, nil)
	mockStore.On("Get", mock.Anything, &sibling, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &grandchild}}}, nil)
	mockStore.On("Get", mock.Anything, &grandchild, mock.Anything).
		Return(&meta.Data{}, nil)

	children, err := GetConflictingChildrenFromRoots(context.Background(), mockStore, []chainhash.Hash{root}, make(map[chainhash.Hash]struct{}), 0)
	require.NoError(t, err)
	require.ElementsMatch(t, []chainhash.Hash{goneErr, goneNil, sibling, grandchild}, children)

	t.Run("any other read error aborts", func(t *testing.T) {
		failing := &MockUtxostore{}
		failing.On("Get", mock.Anything, &root, mock.Anything).
			Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &sibling}}}, nil)
		failing.On("Get", mock.Anything, &sibling, mock.Anything).
			Return(nil, errors.NewStorageError("store down"))

		_, err := GetConflictingChildrenFromRoots(context.Background(), failing, []chainhash.Hash{root}, make(map[chainhash.Hash]struct{}), 0)
		require.ErrorIs(t, err, errors.ErrStorageError)
	})
}

// The frozen sentinel is not a record. As a root it is skipped; reached as a
// child it is returned for the callers' frozen checks but never read.
func TestGetConflictingChildrenFromRoots_FrozenSentinel(t *testing.T) {
	root := createTestHash("frozen-roots-root")

	mockStore := &MockUtxostore{}
	mockStore.On("Get", mock.Anything, &root, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &subtree.FrozenBytesTxHash}}}, nil)

	children, err := GetConflictingChildrenFromRoots(context.Background(), mockStore,
		[]chainhash.Hash{subtree.CoinbasePlaceholderHashValue, root}, make(map[chainhash.Hash]struct{}), 0)
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{subtree.FrozenBytesTxHash}, children)

	mockStore.AssertNumberOfCalls(t, "Get", 1)
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &subtree.FrozenBytesTxHash, mock.Anything)
}
