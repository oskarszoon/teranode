// Package blockchain provides functionality for managing the Bitcoin blockchain.
package blockchain

import (
	"context"
	"net/http"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/looplab/fsm"
)

// FSMTransitions is the single source of truth for blockchain FSM transitions.
// Used by NewFiniteStateMachine and by AvailableEventsForState.
var FSMTransitions = fsm.Events{
	{
		Name: blockchain_api.FSMEventType_RUN.String(),
		Src: []string{
			blockchain_api.FSMStateType_IDLE.String(),
			blockchain_api.FSMStateType_CATCHINGBLOCKS.String(),
		},
		Dst: blockchain_api.FSMStateType_RUNNING.String(),
	},
	{
		// IDLE stays a source so an operator who idled a partially-synced node can
		// resume it by catching up rather than by going straight to RUNNING. That
		// matters because RUN from IDLE is exempt from the checkpoint gate (see
		// Server.SendFSMEvent): if this were the only way out of IDLE, resuming an
		// idled node would mean enabling live subtree validation and block-assembly
		// tx feeding below the checkpoint, which is exactly what routing catchup
		// through CATCHINGBLOCKS exists to avoid.
		//
		// Keeping this edge does not let a block announcement quietly revive an
		// idled node. That is enforced at the caller instead, in
		// processCatchupChItem, which drops the item while the FSM reads IDLE. The
		// FSM stays permissive about the transition; the automatic path is the one
		// that has to be polite about it.
		Name: blockchain_api.FSMEventType_CATCHUPBLOCKS.String(),
		Src: []string{
			blockchain_api.FSMStateType_IDLE.String(),
			blockchain_api.FSMStateType_RUNNING.String(),
		},
		Dst: blockchain_api.FSMStateType_CATCHINGBLOCKS.String(),
	},
	{
		// STOP is the operator's "stop what you are doing and go idle" request.
		// CATCHINGBLOCKS is a source because IDLE is what the destructive-recovery
		// tooling gates on (cmd/rewindblockchain preflight), and on v0.16 a node
		// spends its whole initial block download in CATCHINGBLOCKS. Without this
		// edge the only route to IDLE is via RUNNING, which briefly switches on
		// live subtree validation and block-assembly tx feeding on a node that is
		// not caught up - exactly what routing catchup through CATCHINGBLOCKS
		// avoids.
		//
		// This does not cancel an in-flight catchup, and IDLE is not a state the
		// node is guaranteed to hold: see the note on the CATCHINGBLOCKS guard in
		// Server.SendFSMEvent for what can still take it out of IDLE.
		Name: blockchain_api.FSMEventType_STOP.String(),
		Src: []string{
			blockchain_api.FSMStateType_RUNNING.String(),
			blockchain_api.FSMStateType_CATCHINGBLOCKS.String(),
		},
		Dst: blockchain_api.FSMStateType_IDLE.String(),
	},
}

// AvailableEventsForState returns the event names valid from the given state,
// derived from FSMTransitions (single source of truth). Order follows the
// table's declaration order. Unknown state returns an empty (non-nil) slice.
func AvailableEventsForState(state string) []string {
	events := make([]string, 0)
	for _, e := range FSMTransitions {
		for _, src := range e.Src {
			if src == state {
				events = append(events, e.Name)
				break
			}
		}
	}
	return events
}

// NewFiniteStateMachine creates a new finite state machine for the blockchain service.
//
// States: IDLE, RUNNING, CATCHINGBLOCKS
// Events: RUN, CATCHUPBLOCKS, STOP
//
// Automatically sends notifications on state transitions and updates Prometheus metrics.
func (b *Blockchain) NewFiniteStateMachine(opts ...func(*fsm.FSM)) *fsm.FSM {
	// Define callbacks
	callbacks := fsm.Callbacks{
		"enter_state": func(_ context.Context, e *fsm.Event) {
			metadata := map[string]string{
				"event":       e.Event,
				"destination": e.Dst,
			}

			if _, err := b.SendNotification(context.Background(), &blockchain_api.Notification{
				Type:     model.NotificationType_FSMState,
				Hash:     (&chainhash.Hash{})[:], // not relevant for FSMEvent notifications
				Base_URL: "",                     // not relevant for FSMEvent notifications
				Metadata: &blockchain_api.NotificationMetadata{
					Metadata: metadata,
				},
			}); err != nil {
				b.logger.Errorf("[Blockchain][FiniteStateMachine] error sending notification: %s", err)
			}

			prometheusBlockchainFSMCurrentState.Set(float64(blockchain_api.FSMStateType_value[e.Dst]))
		},
	}

	// Create the finite state machine, with states and transitions
	finiteStateMachine := fsm.NewFSM(
		blockchain_api.FSMStateType_IDLE.String(),
		FSMTransitions,
		callbacks,
		// fsm.Callbacks{},
	)

	// apply options
	for _, opt := range opts {
		opt(finiteStateMachine)
	}

	return finiteStateMachine
}

// CheckFSM creates a health check function for the blockchain FSM.
// Returns a function that checks the current FSM state and returns appropriate
// HTTP status codes:
//   - StatusOK (200): For CATCHINGBLOCKS, RUNNING states
//   - StatusServiceUnavailable (503): For IDLE state
func CheckFSM(blockchainClient ClientI) func(ctx context.Context, checkLiveness bool) (int, string, error) {
	return func(ctx context.Context, checkLiveness bool) (int, string, error) {
		state, err := blockchainClient.GetFSMCurrentState(ctx)
		if err != nil {
			return http.StatusServiceUnavailable, "failed to check FSM state", err
		}

		var (
			status int
		)

		switch *state {
		case blockchain_api.FSMStateType_CATCHINGBLOCKS:
			status = http.StatusOK
		case blockchain_api.FSMStateType_RUNNING:
			status = http.StatusOK
		case blockchain_api.FSMStateType_IDLE:
			status = http.StatusOK
		default:
			status = http.StatusServiceUnavailable
		}

		return status, state.String(), nil
	}
}
