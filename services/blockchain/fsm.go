// Package blockchain provides functionality for managing the Bitcoin blockchain.
package blockchain

import (
	"context"
	"net/http"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/looplab/fsm"
)

// FSM publication is normally immediate. A stopped subscription manager must
// not hold the transition lock indefinitely after the state has been persisted.
const fsmNotificationEnqueueTimeout = time.Second

// fsmTransitionIntent identifies the only safe replay after an ambiguous write.
// Every field is protected by fsmMu; the pointer itself is the retry generation.
type fsmTransitionIntent struct {
	source      string
	event       string
	destination string
}

const fsmPersistenceRetryInterval = 250 * time.Millisecond

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
		Name: blockchain_api.FSMEventType_CATCHUPBLOCKS.String(),
		Src: []string{
			blockchain_api.FSMStateType_IDLE.String(),
			blockchain_api.FSMStateType_RUNNING.String(),
		},
		Dst: blockchain_api.FSMStateType_CATCHINGBLOCKS.String(),
	},
	{
		Name: blockchain_api.FSMEventType_STOP.String(),
		Src: []string{
			blockchain_api.FSMStateType_RUNNING.String(),
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
// Persists accepted transitions before changing state, sending notifications,
// and updating Prometheus metrics. Runtime events must use SendFSMEvent, which
// serializes admission and detaches the transition from caller cancellation.
// A nil receiver supports visualization only; firing events requires an
// initialized Blockchain with settings and a store.
func (b *Blockchain) NewFiniteStateMachine(opts ...func(*fsm.FSM)) *fsm.FSM {
	// Define callbacks
	callbacks := fsm.Callbacks{
		"before_event": func(ctx context.Context, e *fsm.Event) {
			storeCtx, cancel := b.fsmStoreContext(context.WithoutCancel(ctx))
			defer cancel()
			if err := b.store.SetFSMState(storeCtx, e.Dst); err != nil {
				// Cancel before looplab installs its pending transition or changes
				// state, so an explicit retry remains possible. A failed write may
				// have committed: never attempt an unsafe compensating rollback.
				b.fsmPersistenceUncertain = true
				if b.fsmPendingIntent == nil {
					b.fsmPendingIntent = &fsmTransitionIntent{source: e.Src, event: e.Event, destination: e.Dst}
				}
				b.startFSMRetryLocked()
				b.logger.Errorf("[Blockchain][FiniteStateMachine] Failed to persist %s -> %s; in-memory state remains %s; database may already contain %s: %v", e.Src, e.Dst, e.Src, e.Dst, err)
				e.Cancel(errors.NewStorageError("failed to persist FSM transition from %s to %s", e.Src, e.Dst, err))
				return
			}
			b.fsmPersistenceUncertain = false
			b.fsmPendingIntent = nil
		},
		"enter_state": func(_ context.Context, e *fsm.Event) {
			metadata := map[string]string{
				"event":       e.Event,
				"destination": e.Dst,
			}

			notification := &blockchain_api.Notification{
				Type:     model.NotificationType_FSMState,
				Hash:     (&chainhash.Hash{})[:], // not relevant for FSMEvent notifications
				Base_URL: "",                     // not relevant for FSMEvent notifications
				Metadata: &blockchain_api.NotificationMetadata{
					Metadata: metadata,
				},
			}
			timer := time.NewTimer(fsmNotificationEnqueueTimeout)
			defer timer.Stop()
			select {
			case b.notifications <- notification:
			case <-timer.C:
				// The durable state is authoritative. Retain ordered publication
				// before admitting a different transition.
				b.fsmNotificationPending = notification
				go b.retryFSMNotification(notification)
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

// startFSMRetryLocked runs one AppCtx-bound reconciler. Tests without an
// application lifecycle may retry the exact event synchronously instead.
func (b *Blockchain) startFSMRetryLocked() {
	if b.fsmRetryRunning || b.AppCtx == nil || b.AppCtx.Err() != nil {
		return
	}
	b.fsmRetryRunning = true
	go b.retryUncertainFSMTransition()
}

func (b *Blockchain) retryUncertainFSMTransition() {
	for {
		timer := time.NewTimer(fsmPersistenceRetryInterval)
		select {
		case <-b.AppCtx.Done():
			timer.Stop()
			b.fsmMu.Lock()
			b.fsmRetryRunning = false
			b.fsmMu.Unlock()
			return
		case <-timer.C:
		}

		b.fsmMu.Lock()
		intent := b.fsmPendingIntent
		if intent == nil || b.AppCtx.Err() != nil {
			b.fsmRetryRunning = false
			b.fsmMu.Unlock()
			return
		}
		if b.finiteStateMachine.Current() == intent.source && b.fsmNotificationPending == nil {
			// sendFSMEventLocked reapplies the RUN checkpoint gate. Its callback
			// retains this exact pointer on failure and clears it only after an
			// acknowledged store write.
			_, _ = b.sendFSMEventLocked(b.AppCtx, &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType(blockchain_api.FSMEventType_value[intent.event])})
		}
		b.fsmMu.Unlock()
	}
}

func (b *Blockchain) retryFSMNotification(notification *blockchain_api.Notification) {
	ctx := b.AppCtx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case b.notifications <- notification:
		b.fsmMu.Lock()
		if b.fsmNotificationPending == notification {
			b.fsmNotificationPending = nil
		}
		b.fsmMu.Unlock()
	case <-ctx.Done():
		// Shutdown discards the in-process queue. Startup restores persisted FSM
		// state and the subscription manager's initial-state publication.
	}
}

// CheckFSM creates a health check function for the blockchain FSM.
// Returns a function that checks the current FSM state and returns appropriate
// HTTP status codes:
//   - StatusOK (200): For IDLE, RUNNING, CATCHINGBLOCKS states — an idle node is
//     healthy but not yet processing, not unavailable.
//   - StatusServiceUnavailable (503): For any unknown/unlisted state, or if the
//     FSM state query itself fails.
//
// checkLiveness is accepted only to satisfy the shared health.Check signature; this
// check ignores it because it is registered as a readiness check. A liveness probe
// does still reach every service's Health, but those implementations pass no checks
// on that path, so this one is never assembled into the list.
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
