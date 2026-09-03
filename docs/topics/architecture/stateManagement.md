# 🗂️️ State Management in Teranode

1. [Introduction](#1-introduction)
2. [State Machine in Teranode](#2-state-machine-in-teranode)
3. [Functionality](#3-functionality)
    - [3.1. State Machine Initialization](#31-state-machine-initialization)
    - [3.2. Accessing the State Machine](#32-accessing-the-state-machine)
    - [3.2.1. Access via Command-Line Interface](#321-access-via-command-line-interface-recommended)
    - [3.2.2. Access via HTTP](#322-access-via-http-asset-server)
    - [3.2.3. Access via gRPC](#323-access-via-grpc)
    - [3.3. State Machine States](#33-state-machine-states)
    - [3.3.1. FSM: Idle State](#331-fsm-idle-state)
    - [3.3.2. FSM: Running State](#332-fsm-running-state)
    - [3.3.3. FSM: Catching Blocks State](#333-fsm-catching-blocks-state)
    - [3.4. State Machine Events](#34-state-machine-events)
    - [3.4.1. FSM Event: Run](#341-fsm-event-run)
    - [3.4.2. FSM Event: Catch up Blocks](#342-fsm-event-catch-up-blocks)
    - [3.4.3. FSM Event: Stop](#343-fsm-event-stop)
    - [3.5. Waiting on State Machine Transitions](#35-waiting-on-state-machine-transitions)
4. [Other Resources](#4-other-resources)

## 1. Introduction

A Finite State Machine is a model used in computer science that describes a system which can be in one of a finite number of states at any given time. The machine can transition between these predefined states based on inputs or conditions (an "event").

Finite State Machines:

- have a finite set of states.
- can only be in one state at a time.
- transition between states based on inputs or events.
- have a defined initial state.
- may have one or more final states.

## 2. State Machine in Teranode

The Teranode blockchain service uses a Finite State Machine (FSM) to manage the various states and transitions of the node. The FSM is responsible for controlling the node's behavior based on the current state and incoming events.

The FSM has the following **states**:

- **Idle**
- **Running**
- **CatchingBlocks**

The FSM responds to the following **events**:

- **Run**
- **CatchupBlocks**
- **Stop**

The diagram below represents the relationships between the states and events in the FSM (as defined in `services/blockchain/fsm.go`). Its start arrow shows the state a node actually boots in, which `Init` sets with `SetState` rather than by firing an event; the state machine's own constructor default is `Idle`, which is why the generated `docs/state-machine.diagram.md` starts there instead:

![Finite state machine diagram](img/fsmDiagram.svg){ width="500" }

The FSM handles the following state **transitions**:

- **Run**: Transitions to _Running_ from _Idle_ or _CatchingBlocks_
- **CatchupBlocks**: Transitions to _CatchingBlocks_ from _Running_ or _Idle_
- **Stop**: Transitions to _Idle_ from _Running_ or _CatchingBlocks_

An operator leaving _Idle_ has two routes, and they are not equivalent.
**CatchupBlocks** resumes downloading and leaves the checkpoint gate in force,
so the node cannot go live until its tip has caught up. **Run** goes live
immediately. Use **CatchupBlocks** to resume a node that was idled part-way
through its initial sync — **Run** will be refused, because the checkpoint gate
exempts only a node with no chain tip at all, not one with a partial chain.

Block announcements are stopped from using the _Idle_ to _CatchingBlocks_ edge
to revive an idled node: block validation drops incoming catch-up work while the
FSM reads _Idle_ (`processCatchupChItem`). The restriction is on that automatic
path rather than on the transition, so the operator keeps the safe resume route.

That check is best-effort by design. If the state read itself fails it lets the
catch-up proceed rather than stalling sync, on the grounds that a failed read is
far likelier than the node having been idled. So it makes an accidental revival
unlikely, not impossible — which is one more reason to re-read the state before
starting anything destructive.

Teranode provides a visualizer tool to generate and visualize the state machine diagram. To run the visualizer, use the command `go run services/blockchain/fsm_visualizer/main.go`. The generated `docs/state-machine.diagram.md` can be visualized using <https://mermaid.live/>.

## 3. Functionality

### 3.1. State Machine Initialization

As part of its own initialization, the Blockchain service restores the FSM to the state it last persisted. A node with no persisted state (a fresh node) starts in **CatchingBlocks** so that it begins downloading blocks immediately rather than waiting to be moved out of **Idle**; it is promoted to **Running** only once catch-up completes above the network's highest checkpoint.

### 3.2. Accessing the State Machine

#### 3.2.1. Access via Command-Line Interface (Recommended)

The Teranode Command-Line Interface (teranode-cli) provides the most direct and recommended approach for interacting with the State Machine. The CLI abstracts the underlying API calls and offers a straightforward interface for both operators and developers.

The CLI provides two primary commands for FSM interaction:

- **getfsmstate** - Queries and displays the current state of the FSM
- **setfsmstate** - Changes the FSM state by sending the appropriate event

These commands interface with the same underlying mechanisms as the gRPC methods, but provide a more user-friendly experience with appropriate validation and feedback.

#### 3.2.2. Access via HTTP (Asset Server)

The Asset Server provides a RESTful HTTP interface to the State Machine, offering a web-friendly approach to FSM interaction. This interface is particularly useful for web applications and administrative dashboards that need to monitor or control node state.

The Asset Server exposes the following endpoints for FSM interaction:

- **GET /api/v1/fsm/state** - Retrieves the current FSM state
- **POST /api/v1/fsm/state** - Sends a custom event to the FSM
- **GET /api/v1/fsm/events** - Lists all available FSM events
- **GET /api/v1/fsm/states** - Lists all possible FSM states

These HTTP endpoints provide the same functionality as the CLI and gRPC methods but with a RESTful interface that can be accessed using standard HTTP clients.

#### 3.2.3. Access via gRPC

The Blockchain service also exposes the following gRPC methods to interact with the FSM programmatically:

- **GetFSMCurrentState** - Returns the current state of the FSM
- **SendFSMEvent** - Sends an event to the FSM to trigger a state transition
- **Run** - Transitions the FSM to the Running state (delegates on the SendFSMEvent method)
- **CatchUpBlocks** - Transitions the FSM to the CatchingBlocks state (delegates on the SendFSMEvent method)
- **Idle** - Transitions the FSM to the Idle state by sending the STOP event (delegates on the SendFSMEvent method)

### 3.3. State Machine States

#### 3.3.1. FSM: Idle State

A node reaches `Idle` either by being stopped from `Running`, or by having
persisted `Idle` before a restart. A fresh node no longer starts here — it starts
in `CatchingBlocks` (see section 3.1). In this state:

- No operations are permitted
- All services are inactive
- The node is not participating in the network in any way
- Must be manually triggered to transition to another state

Allowed Operations in Idle State:

- ❌ Process external transactions
- ❌ Legacy relay transactions
- ❌ Queue subtrees
- ❌ Process subtrees
- ❌ Queue blocks
- ❌ Process blocks
- ❌ Relay blocks
- ❌ Speedy process blocks
- ❌ Create subtrees (or propagate them)
- ❌ Create blocks (mine candidates)

Services wait for the FSM to leave `Idle` before starting their operations — any
non-Idle state, including `CatchingBlocks`, releases them (see section 3.5). As
such, the node should see no activity for as long as the FSM stays in `Idle`.

The node can also return back to the `Idle` state from either `Running` or
`CatchingBlocks`, however this can only be triggered by a manual / external
request.

Returning to `Idle` is not the same as the node having booted into it. The list
above describes a node that has not yet started: services block on the
transition out of `Idle` once, at startup, and do not re-suspend if a running
node is later set back to `Idle`. A running node returned to `Idle` will not
pick up new work on the Teranode path — incoming catch-up work is dropped while
the FSM reads `Idle`, and the sync coordinator drives sync from `Running` and
`CatchingBlocks` but never from `Idle` — but a catchup already in flight drains
to completion rather than being cancelled. If that catchup completes after the
node was idled, it may promote the node back to `Running`. A node running the
legacy sync service has a further, larger exception. Both are covered in
§3.4.3. `Idle` is primarily a
precondition for operator tooling that requires the node to be quiet (notably
`teranode-cli rewindblockchain`), not a live pause switch.

#### 3.3.2. FSM: Running State

The `Running` state represents the node actively participating in the network. In this state:

Allowed Operations in Running State:

- ✅ Process external transactions
- ✅ Legacy relay transactions
- ✅ Queue subtrees
- ✅ Process subtrees
- ✅ Queue blocks
- ✅ Process blocks
- ✅ Relay blocks
- ❌ Speedy process blocks
- ✅ Create subtrees (or propagate them)
- ✅ Create blocks (mine candidates)

Once the FSM transitions to the `Running` state, all services will start their normal operations.

The Block Assembler will only mine blocks when the node is in the `Running` state. The Block Assembler will never mine blocks under any other node state.

#### 3.3.3. FSM: Catching Blocks State

The `CatchingBlocks` state represents the node catching up on blocks. It is entered
either by BlockValidation, when a running node finds it has fallen behind the
network, or at startup, because a fresh node with no persisted state boots
straight into it (see section 3.1). In this state:

Allowed Operations in Catching Blocks State:

- ✅ Process external transactions
- ✅ Legacy relay transactions
- ✅ Queue subtrees
- ✅ Process subtrees
- ✅ Queue blocks
- ✅ Process blocks
- ✅ Relay blocks
- ❌ Speedy process blocks
- ❌ Create subtrees (or propagate them)
- ❌ Create blocks (mine candidates)

Outbound P2P gossip is gated per FSM state by a declarative allow-list
(`outboundTopicsAllowed` in `services/p2p/publish_gate.go`): in `Running` the
node may publish block, subtree, rejected-tx, and `node_status` messages; in
`CatchingBlocks` and `Idle` it publishes only `node_status` (so peers can track
its height). `Idle` is deliberately restrictive because it doubles as the
blockchain client's safety fallback: when the FSM state cannot be fetched or
the heartbeat is lost, the client caches `Idle` and reports it with no error,
so a degraded blockchain client reads as `Idle` and gossip stops (only
`node_status` keeps flowing) until the state is re-fetched. An idle node must
not participate in the network in any case. States unknown to the
allow-list (e.g. from a newer blockchain service) also fall back to
`node_status`-only. Suppressed publishes are counted in the
`teranode_p2p_publish_blocked_total` metric.

##### Error Handling in Catching Blocks State

When an error occurs during the catchup process, the FSM behavior has been updated to maintain state consistency:

![fsm_catchup_error_handling.svg](img/plantuml/fsm_catchup_error_handling.svg)

Key points about error handling:

- **State Persistence**: When an error occurs during catchup (e.g., validation failure, network error), the FSM **remains** in the `CatchingBlocks` state
- **No Automatic Reversion**: The FSM does **not** automatically revert to the `Running` state on error
- **Explicit Recovery Required**: Recovery from errors requires either:
    - Manual retry of the catchup process
    - Automatic retry mechanism (if configured)
    - Explicit state reset via operator intervention
- **Consistency**: This behavior prevents inconsistent state transitions and ensures the node doesn't incorrectly resume normal operations while catchup is incomplete

### 3.4. State Machine Events

#### 3.4.1. FSM Event: Run

The gRPC `Run` method triggers the FSM to transition to the `Running` state. This event is used to indicate that the node is ready to start participating in the network and processing transactions and blocks.

![fsm_run.svg](img/plantuml/fsm_run.svg)

#### 3.4.2. FSM Event: Catch up Blocks

The gRPC `CatchUpBlocks` method triggers the FSM to transition to the `CatchingBlocks` state. This event is used to indicate that the node is catching up on blocks and needs to process the latest blocks before resuming full operations.

![fsm_catchup_blocks.svg](img/plantuml/fsm_catchup_blocks.svg)

#### 3.4.3. FSM Event: Stop

The gRPC `Idle` method sends a `Stop` event to the FSM, which triggers a transition to the `Idle` state. It marks the node as having been told to stop taking on new work: no new catch-up can start from `Idle`, and the sync coordinator drives sync only from `Running` and `CatchingBlocks`. It does not halt work already under way — a catch-up in flight drains to completion, and the other services check for `Idle` only at startup, so they do not re-suspend when a running node returns to it.

`Stop` is accepted from both `Running` and `CatchingBlocks`. The
`CatchingBlocks` source matters operationally: a node spends its entire initial
block download in that state, and `Idle` is what `teranode-cli rewindblockchain`
gates on, so without it the only route to `Idle` would be through `Running` —
which briefly enables live subtree validation and block-assembly transaction
feeding on a node that is not caught up.

See the caveats in the `Idle` state section above for what `Stop` does and does
not halt.

`Idle` is not a state the node is guaranteed to hold, and this matters if you
are idling a node in order to rewind it. Two things can take it back out.

The narrow one is a race at catch-up completion. Block validation restores the
FSM by reading the current state and then sending `Run` as two separate calls, so
a `Stop` landing between those steps leaves the `Run` executing from `Idle`,
which is accepted. The window is one instant.

The larger one applies to nodes running the legacy sync service, and is not a
race at all. `services/legacy/netsync/manager.go` sends `Run` from three places
without checking for `Idle` first: `startSync`, which is reached from a ticker
and fires whenever the chosen sync peer reports the same height as the node;
`handleBlockMsg`, on every accepted block once the node considers itself current;
and the `blockHandler` loop, which fires precisely because the state is not
`Running`. On such a node an operator's `Stop` is undone on the next tick, with
no race needed.

Both paths reach `SendFSMEvent` with prior state `Idle`, so on a node below the
highest checkpoint they take the exempt branch and log `RUN accepted from IDLE
despite the checkpoint gate (operator override)` with no operator involved. Read
that line with the call site in mind rather than assuming a human acted.

So: re-read the state after idling a node, and again immediately before starting
anything destructive.

### 3.5. Waiting on State Machine Transitions

Through internal helper methods, services can wait for the FSM to transition out of the `Idle` state before proceeding with their operations. This method is used by various services to ensure that the node is in the correct state before starting their activities.

The method blocks until the FSM transitions from the `Idle` state to any non-Idle state (such as `Running` or `CatchingBlocks`) or until a timeout occurs. This ensures that services are synchronized with the node's state changes and can respond accordingly.

The following services wait for the FSM to transition from the `Idle` state before starting their operations:

- Asset Server
- Block Persister
- Block Validation
- Legacy P2P Gateway
- P2P
- Propagation
- Pruner
- Subtree Validation
- UTXO Persister
- Validator

---

## 4. Other Resources

### How-to Guides

- [How to Interact with the FSM](../../howto/miners/minersHowToInteractWithFSM.md) - Practical guide for managing FSM states in test and production environments (Docker and Kubernetes)

### API References

- [Blockchain API Reference](../../references/services/blockchain_reference.md) - Complete reference for the Blockchain service API, including FSM methods
- [Asset Server API Reference](../../references/services/asset_reference.md) - Reference for the Asset Server REST API, including FSM endpoints
