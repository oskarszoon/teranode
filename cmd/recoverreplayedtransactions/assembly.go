package recoverreplayedtransactions

import (
	"context"

	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
)

// NewAssembly adapts the optional recovery RPC interface. Construct the normal
// blockassembly.Client with node settings and close it after use.
func NewAssembly(client blockassembly.RecoveryClient) replayrecovery.Assembly {
	return &assemblyAdapter{client: client}
}

type assemblyAdapter struct{ client blockassembly.RecoveryClient }

func (a *assemblyAdapter) State(ctx context.Context) (replayrecovery.AssemblyState, error) {
	return a.client.RecoveryState(ctx)
}
func (a *assemblyAdapter) Reset(ctx context.Context) (replayrecovery.AssemblyState, error) {
	return a.client.RecoveryReset(ctx)
}
func (a *assemblyAdapter) Transactions(ctx context.Context, visit func(string) error) (replayrecovery.AssemblyState, error) {
	return a.client.RecoveryTransactions(ctx, false, visit)
}
func (a *assemblyAdapter) Candidate(ctx context.Context, visit func(string) error) (replayrecovery.AssemblyState, error) {
	return a.client.RecoveryTransactions(ctx, true, visit)
}
