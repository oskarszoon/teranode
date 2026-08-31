package blockchain

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	blockchain_store "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// blockAssemblerStateKey mirrors blockassembly.StateKey. It is duplicated here
// rather than imported because services/blockassembly depends on this package,
// so importing it back would be a cycle.
const blockAssemblerStateKey = "BlockAssembler"

// newBootStateBlockchain builds a Blockchain backed by a real sqlitememory
// blockchain store (per AGENTS.md: do not mock the blockchain store) with the
// supplied blockchain_initializeNodeInState value.
func newBootStateBlockchain(t *testing.T, initializeNodeInState string) (*Blockchain, blockchain_store.Store) {
	t.Helper()

	storeURL, err := url.Parse("sqlitememory://")
	require.NoError(t, err)

	tSettings := &settings.Settings{ChainCfgParams: &chaincfg.RegressionNetParams}
	tSettings.BlockChain.InitializeNodeInState = initializeNodeInState

	store, err := blockchain_store.NewStore(ulogger.TestLogger{}, storeURL, tSettings)
	require.NoError(t, err)

	b, err := New(context.Background(), ulogger.TestLogger{}, tSettings, store, nil)
	require.NoError(t, err)

	return b, store
}

// TestInit_FreshNodeBootState pins the fresh-node boot state for every accepted
// value of blockchain_initializeNodeInState. The empty case is the #1135
// regression pin: a fresh install with no configured state must still boot
// straight into catch-up rather than sitting idle for minutes.
func TestInit_FreshNodeBootState(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       blockchain_api.FSMStateType
	}{
		{
			name:       "unset boots CATCHINGBLOCKS",
			configured: "",
			want:       blockchain_api.FSMStateType_CATCHINGBLOCKS,
		},
		{
			name:       "IDLE boots quiescent for seed verification",
			configured: blockchain_api.FSMStateType_IDLE.String(),
			want:       blockchain_api.FSMStateType_IDLE,
		},
		{
			name:       "CATCHINGBLOCKS can be requested explicitly",
			configured: blockchain_api.FSMStateType_CATCHINGBLOCKS.String(),
			want:       blockchain_api.FSMStateType_CATCHINGBLOCKS,
		},
		{
			name:       "RUNNING is permitted and warned about",
			configured: blockchain_api.FSMStateType_RUNNING.String(),
			want:       blockchain_api.FSMStateType_RUNNING,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			b, store := newBootStateBlockchain(t, tt.configured)

			// Precondition: a fresh store has no persisted FSM state, which is
			// what routes Init down the fresh-node branch.
			persisted, err := store.GetFSMState(ctx)
			require.NoError(t, err)
			require.Empty(t, persisted, "fresh store must have no persisted FSM state")

			require.NoError(t, b.Init(ctx))

			require.Equal(t, tt.want.String(), b.finiteStateMachine.Current())

			// The chosen state must be persisted, so an operator who restarts
			// services part-way through verifying a seed does not find the node
			// has moved on to catching up.
			persisted, err = store.GetFSMState(ctx)
			require.NoError(t, err)
			require.Equal(t, tt.want.String(), persisted)
		})
	}
}

// TestInit_InvalidInitializeNodeInState covers the fail-loud requirement: a
// value that is not an FSM state name must abort startup (Init's error
// propagates through ServiceManager.AddService) rather than silently falling
// back to the default.
func TestInit_InvalidInitializeNodeInState(t *testing.T) {
	for _, configured := range []string{"idle", "Running", "FOO", "LEGACYSYNCING", "1"} {
		t.Run(configured, func(t *testing.T) {
			b, _ := newBootStateBlockchain(t, configured)

			err := b.Init(context.Background())
			require.Error(t, err)
			require.Contains(t, err.Error(), "blockchain_initializeNodeInState")
			require.Contains(t, err.Error(), configured)
			// The error must name the accepted values, not just reject.
			require.Contains(t, err.Error(), blockchain_api.FSMStateType_IDLE.String())
			require.Contains(t, err.Error(), blockchain_api.FSMStateType_CATCHINGBLOCKS.String())
		})
	}
}

// TestInit_InvalidValueFailsEvenWithPersistedState is why validation is
// unconditional. A typo on a node that already has a persisted state would
// otherwise lie dormant until the next fresh boot — for a node that was just
// reseeded, the worst possible moment to discover it.
func TestInit_InvalidValueFailsEvenWithPersistedState(t *testing.T) {
	ctx := context.Background()
	b, store := newBootStateBlockchain(t, "nonsense")

	require.NoError(t, store.SetFSMState(ctx, blockchain_api.FSMStateType_RUNNING.String()))

	err := b.Init(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "blockchain_initializeNodeInState")
}

// TestInit_PersistedStateIgnoresConfiguredBootState pins that the setting only
// ever fills the fresh-node case. A node that has been running must resume what
// it persisted, whatever the config says.
func TestInit_PersistedStateIgnoresConfiguredBootState(t *testing.T) {
	ctx := context.Background()
	b, store := newBootStateBlockchain(t, blockchain_api.FSMStateType_IDLE.String())

	require.NoError(t, store.SetFSMState(ctx, blockchain_api.FSMStateType_RUNNING.String()))

	require.NoError(t, b.Init(ctx))

	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), b.finiteStateMachine.Current(),
		"persisted state must win over blockchain_initializeNodeInState")

	persisted, err := store.GetFSMState(ctx)
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted)
}

// TestInit_LocalTestStartStateWinsOverConfiguredBootState pins the precedence
// between the two overrides. -localTestStartFromState forces its state even
// over a persisted one, so it also outranks the operator setting.
func TestInit_LocalTestStartStateWinsOverConfiguredBootState(t *testing.T) {
	ctx := context.Background()
	b, store := newBootStateBlockchain(t, blockchain_api.FSMStateType_IDLE.String())
	b.localTestStartState = blockchain_api.FSMStateType_RUNNING.String()

	require.NoError(t, b.Init(ctx))

	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), b.finiteStateMachine.Current())

	persisted, err := store.GetFSMState(ctx)
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted)
}

// TestInit_SeededStoreBootsIntoConfiguredState is the seeded-node path, the case
// this setting exists for. cmd/seeder writes the UTXO set, headers and
// state["BlockAssembler"] straight into the stores but deliberately never writes
// state["fsm_state"], so the first start after a seed takes the fresh-node
// branch. With the operator default of IDLE the node must come up quiescent so
// the snapshot can be verified before it touches the network.
func TestInit_SeededStoreBootsIntoConfiguredState(t *testing.T) {
	ctx := context.Background()
	b, store := newBootStateBlockchain(t, blockchain_api.FSMStateType_IDLE.String())

	// Stand in for what a completed seed leaves behind: BlockAssembler state
	// present, FSM state absent.
	require.NoError(t, store.SetState(ctx, blockAssemblerStateKey, []byte("seeded-checkpoint")))

	persisted, err := store.GetFSMState(ctx)
	require.NoError(t, err)
	require.Empty(t, persisted, "a seeded store must not carry an FSM state")

	require.NoError(t, b.Init(ctx))

	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), b.finiteStateMachine.Current())

	// The seeder's BlockAssembler state must be untouched by Init.
	baState, err := store.GetState(ctx, blockAssemblerStateKey)
	require.NoError(t, err)
	require.Equal(t, []byte("seeded-checkpoint"), baState)
}

// TestFSMBootState_NilSettings guards the helper against the partially
// constructed Blockchain values several tests in this package build directly.
func TestFSMBootState_NilSettings(t *testing.T) {
	b := &Blockchain{logger: ulogger.TestLogger{}}

	got, err := b.fsmBootState()
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_CATCHINGBLOCKS.String(), got)
}
