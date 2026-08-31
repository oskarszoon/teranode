# How to Manage Teranode States

This guide explains how to change and monitor Teranode's state.

The state a fresh instance boots into depends on its settings context:

| Context | Fresh-node boot state | Why |
|---------|----------------------|-----|
| `operator` (and its sub-contexts), `docker.m` | `IDLE` | Production deployments come up quiescent so you can verify a seed before the node consumes network or mutates state. |
| everything else (`dev`, `test`, `docker`, `docker.ci`, e2e stacks) | `CATCHINGBLOCKS` | Local iteration and CI start catching up with no manual step. |

This is controlled by `blockchain_initializeNodeInState`, which accepts `IDLE`,
`RUNNING` or `CATCHINGBLOCKS`; an empty value means `CATCHINGBLOCKS` and an
unrecognised value fails startup. Override it in `settings_local.conf` for your
context. A restarted instance resumes whatever state it last persisted and
ignores the setting entirely.

## Prerequisites

- Access to a running Teranode instance
- One of the following access methods:
    - Admin Dashboard (easiest - web-based interface)
    - `teranode-cli` (recommended for scripting - available in all Teranode containers)
    - `grpcurl` (advanced - requires network access to the RPC Server on port 18087)

## Recommended Method: Using Admin Dashboard

The Admin Dashboard provides the easiest way to view and manage Teranode FSM states through a web interface.

### Accessing the Dashboard

**Docker Compose:**

```bash
# Access the dashboard in your browser
# http://localhost:8090/admin
```

**Kubernetes:**

```bash
# Port-forward the asset service
kubectl port-forward -n teranode-operator service/asset 8090:8090

# Then access http://localhost:8090/admin in your browser
```

### Managing FSM State

1. Navigate to the FSM State section in the dashboard
2. View the current state
3. Use the state transition controls to change states
4. Monitor state transition logs in real-time

> **Note:** The dashboard must be enabled via the `dashboard_enabled` setting and may require authentication depending on your configuration (`dashboard.auth.enabled`).

## Alternative Method: Using teranode-cli

The `teranode-cli` is recommended for scripting and automation. It provides a command-line interface that works directly with the blockchain service.

### Docker Compose Environment

#### 1. Check Current State

```bash
docker exec -it blockchain teranode-cli getfsmstate
```

#### 2. Set New State

```bash
docker exec -it blockchain teranode-cli setfsmstate --fsmstate RUNNING
```

### Kubernetes Environment

#### 1. Check Current State

Access any Teranode pod and use teranode-cli directly:

```bash
# Get the name of a pod (blockchain or asset are good options)
kubectl get pods -n teranode-operator -l app=blockchain

# Access the pod and run the command
kubectl exec -it <pod-name> -n teranode-operator -- teranode-cli getfsmstate

# Alternative one-liner
kubectl exec -it $(kubectl get pods -n teranode-operator -l app=blockchain -o jsonpath='{.items[0].metadata.name}') -n teranode-operator -- teranode-cli getfsmstate
```

#### 2. Set New State

```bash
# Change state to RUNNING
kubectl exec -it $(kubectl get pods -n teranode-operator -l app=blockchain -o jsonpath='{.items[0].metadata.name}') -n teranode-operator -- teranode-cli setfsmstate --fsmstate RUNNING
```

## Valid FSM States

The following states are valid for all environments:

- IDLE
- RUNNING
- CATCHINGBLOCKS

### When a transition is refused

Two rules constrain which transitions are accepted, and both surface as errors
rather than silent no-ops:

- **Only RUN may leave CATCHINGBLOCKS.** A node that is catching up cannot be
  moved to IDLE; it must finish catching up first.
- **RUN never reaches RUNNING while the chain tip is below the network's highest
  hard-coded checkpoint.** Mainnet and testnet both have checkpoints; regtest has
  none. What happens instead depends on where the node is:
    - **From IDLE**, `setfsmstate running` is routed to CATCHINGBLOCKS. "Run"
      means "put this node into service", and on a node that is not caught up the
      route there is through catch-up. The command reports the state it actually
      reached, so you will see `CATCHINGBLOCKS` rather than `RUNNING` — that is
      the reroute, not a failure. A node seeded at or above the checkpoint goes
      straight to RUNNING instead.
    - **From CATCHINGBLOCKS**, the same RUN is refused with an error naming both
      your tip height and the checkpoint it must reach. The node is already
      catching up and will move to RUNNING once it gets there.

Why the rule exists: going to RUNNING mid-initial-sync lets the mempool and
validator operate under pre-Genesis output rules, and lets the legacy service
relay tx invs that post-Genesis peers ban on sight
(`bad-txns-vout-p2sh BAN THRESHOLD EXCEEDED`).

> **Behaviour change:** the checkpoint rule used to exempt `IDLE -> RUNNING`
> entirely, on the reasoning that a fresh node was never in IDLE. Production
> contexts now boot into IDLE, so the rule applies to every RUN and the IDLE case
> is rerouted as described above. Out-of-tree boot tooling that forces RUNNING on
> a fresh mainnet or testnet node will now land it in CATCHINGBLOCKS instead of
> RUNNING. Regtest has no checkpoints and is unaffected — `setfsmstate running`
> still goes straight to RUNNING there.
>
> **Getting back to IDLE:** there is no `CATCHINGBLOCKS -> IDLE` transition. Once
> a node is catching up, the only way out is RUN. If you need a quiescent node in
> order to inspect it, boot it that way with `blockchain_initializeNodeInState`
> rather than trying to quiesce it after the fact.

## Validation

After each state change, verify the new state:

1. **Admin Dashboard**: View the current state in the FSM State section
2. **teranode-cli**: Use `getfsmstate` command (see above)
3. **Logs**: Check the logs for transition messages
4. **Services**: Verify that expected services are running/stopped according to the state

## Advanced Method: Using grpcurl

For advanced users or automated scripts, you can use `grpcurl` directly. This method requires network access to the blockchain gRPC service on port 18087.

### Docker Compose Environment

Access the blockchain gRPC service directly:

**Check Current State:**

```bash
# Connect to blockchain service on port 18087
grpcurl -plaintext blockchain:18087 blockchain_api.BlockchainAPI.GetFSMCurrentState
```

**Trigger State Transitions:**

```bash
# Transition to RUNNING state
grpcurl -plaintext blockchain:18087 blockchain_api.BlockchainAPI.Run

# Transition to CATCHINGBLOCKS state
grpcurl -plaintext blockchain:18087 blockchain_api.BlockchainAPI.CatchUpBlocks

# Transition to IDLE state
grpcurl -plaintext blockchain:18087 blockchain_api.BlockchainAPI.Idle
```

### Kubernetes Environment

Port-forward the blockchain service:

```bash
# Port forward the blockchain gRPC service
kubectl port-forward -n teranode-operator service/blockchain 18087:18087
```

**Check Current State:**

```bash
grpcurl -plaintext localhost:18087 blockchain_api.BlockchainAPI.GetFSMCurrentState
```

Expected output. A Kubernetes deployment runs the `operator` context, so a fresh
node reports `IDLE`; a restarted node reports whatever state it last persisted:

```json
{
  "state": "IDLE"
}
```

**Trigger State Transitions:**

```bash
# Transition to RUNNING state
grpcurl -plaintext localhost:18087 blockchain_api.BlockchainAPI.Run

# Transition to CATCHINGBLOCKS state
grpcurl -plaintext localhost:18087 blockchain_api.BlockchainAPI.CatchUpBlocks

# Transition to IDLE state
grpcurl -plaintext localhost:18087 blockchain_api.BlockchainAPI.Idle
```

### Wait for State Change

There is no blocking "wait" endpoint. To wait for a specific state, poll the current state until it matches:

```bash
# Poll until the FSM reaches RUNNING
until grpcurl -plaintext localhost:18087 blockchain_api.BlockchainAPI.GetFSMCurrentState \
  | grep -q '"state": "RUNNING"'; do
  sleep 1
done
```

## Further Reading

- [How To Interact With the RPC Server](minersHowToInteractWithRPCServer.md)
- [State Management Documentation](../../topics/architecture/stateManagement.md)
