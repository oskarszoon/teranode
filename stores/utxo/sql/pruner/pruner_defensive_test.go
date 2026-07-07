package pruner

// Coverage for deleteTombstoned's defensive-mode branch. The default-off tests exercise the
// fast delete-all query; this pins the UTXODefensiveEnabled=true path that builds the
// stable-child NOT EXISTS query and binds the safety-window argument.

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/stretchr/testify/require"
)

func defensiveTestSettings() *settings.Settings {
	s := createTestSettings()
	s.Pruner.UTXODefensiveEnabled = true

	return s
}

// deleteTombstoned with defensive mode enabled must run to completion, exercising the
// stable-child verification query builder and the safety-window bind.
func TestDeleteTombstoned_DefensiveModeEnabled(t *testing.T) {
	logger := &MockLogger{}
	db := NewMockDB()

	service, err := NewService(defensiveTestSettings(), Options{
		Logger: logger,
		DB:     db.DB,
	})
	require.NoError(t, err)
	require.True(t, service.defensiveEnabled, "defensive mode must be wired from settings")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	recordsProcessed, err := service.Prune(ctx, 100, "<test-hash>")
	require.NoError(t, err)
	require.GreaterOrEqual(t, recordsProcessed, int64(0))
}
