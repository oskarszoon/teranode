package blockchain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// newMinimalBlockchainForRegistry creates a Blockchain with only the fields
// needed for peer-registry lifecycle tests (peerRegistry, settings, logger).
func newMinimalBlockchainForRegistry(t *testing.T, registryPath string, saveInterval time.Duration) *Blockchain {
	t.Helper()

	return &Blockchain{
		peerRegistry: NewCentralizedPeerRegistry(DefaultBanConfig()),
		settings: &settings.Settings{
			BlockChain: settings.BlockChainSettings{
				PeerRegistryPath:         registryPath,
				PeerRegistrySaveInterval: saveInterval,
			},
		},
		logger: ulogger.NewErrorTestLogger(t),
	}
}

// ---------------------------------------------------------------------------
// Stop saves peer registry when path is configured
// ---------------------------------------------------------------------------

func TestRegistryServer_Stop_SavesRegistryWhenPathConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")

	b := newMinimalBlockchainForRegistry(t, path, time.Minute)

	// Register a peer so there is something to persist.
	b.peerRegistry.Register(&PeerInfo{ID: "stop-peer", Height: 42})
	require.Equal(t, 1, b.peerRegistry.Count())

	// File should not exist before Stop.
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err))

	// Stop should save.
	err = b.Stop(context.Background())
	require.NoError(t, err)

	// File should now exist.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0))

	// Verify the saved data can be loaded back.
	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))
	require.Equal(t, 1, r2.Count())

	got, ok := r2.Get("stop-peer")
	require.True(t, ok)
	require.Equal(t, uint32(42), got.Height)
}

// ---------------------------------------------------------------------------
// Stop does nothing when path is empty (no persistence configured)
// ---------------------------------------------------------------------------

func TestRegistryServer_Stop_NoOpWhenPathEmpty(t *testing.T) {
	b := newMinimalBlockchainForRegistry(t, "", time.Minute)

	b.peerRegistry.Register(&PeerInfo{ID: "ephemeral-peer"})

	// Should succeed without writing anything.
	err := b.Stop(context.Background())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Stop logs warning on save failure (read-only directory)
// ---------------------------------------------------------------------------

func TestRegistryServer_Stop_LogsWarningOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	// Make the directory read-only so the save fails.
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})

	path := filepath.Join(dir, "peers.json")
	b := newMinimalBlockchainForRegistry(t, path, time.Minute)
	b.peerRegistry.Register(&PeerInfo{ID: "peer-1"})

	// Stop should not return an error even if Save fails — it only logs a warning.
	err := b.Stop(context.Background())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// savePeerRegistryPeriodically saves at configured interval
// ---------------------------------------------------------------------------

func TestRegistryServer_SavePeerRegistryPeriodically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "periodic.json")

	b := newMinimalBlockchainForRegistry(t, path, 50*time.Millisecond)
	b.peerRegistry.Register(&PeerInfo{ID: "periodic-peer", Height: 99})

	ctx, cancel := context.WithCancel(context.Background())

	go b.savePeerRegistryPeriodically(ctx, path, 50*time.Millisecond)

	// Wait enough time for at least one tick to fire and save.
	require.Eventually(t, func() bool {
		info, err := os.Stat(path)
		return err == nil && info.Size() > 0
	}, 2*time.Second, 10*time.Millisecond, "periodic save should create the file")

	// Verify file content is valid.
	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))
	require.Equal(t, 1, r2.Count())
	got, ok := r2.Get("periodic-peer")
	require.True(t, ok)
	require.Equal(t, uint32(99), got.Height)

	// Cancel and verify the goroutine exits cleanly.
	cancel()

	// Give the goroutine a moment to terminate.
	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// savePeerRegistryPeriodically exits on context cancellation
// ---------------------------------------------------------------------------

func TestRegistryServer_SavePeerRegistryPeriodically_ExitsOnCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel.json")

	b := newMinimalBlockchainForRegistry(t, path, time.Hour) // long interval

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.savePeerRegistryPeriodically(ctx, path, time.Hour)
		close(done)
	}()

	// Cancel immediately — the goroutine should return.
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("savePeerRegistryPeriodically did not exit after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// savePeerRegistryPeriodically handles save errors gracefully
// ---------------------------------------------------------------------------

func TestRegistryServer_SavePeerRegistryPeriodically_HandlesError(t *testing.T) {
	dir := t.TempDir()
	// Make directory read-only so saves fail.
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})

	path := filepath.Join(dir, "error.json")
	b := newMinimalBlockchainForRegistry(t, path, 50*time.Millisecond)
	b.peerRegistry.Register(&PeerInfo{ID: "peer-1"})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.savePeerRegistryPeriodically(ctx, path, 50*time.Millisecond)
		close(done)
	}()

	// Let several ticks fire — the goroutine should keep running despite errors.
	time.Sleep(200 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// success — goroutine exits cleanly despite repeated save errors
	case <-time.After(2 * time.Second):
		t.Fatal("savePeerRegistryPeriodically did not exit after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// savePeerRegistryPeriodically overwrites previous save
// ---------------------------------------------------------------------------

func TestRegistryServer_SavePeerRegistryPeriodically_Overwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.json")

	b := newMinimalBlockchainForRegistry(t, path, 50*time.Millisecond)
	b.peerRegistry.Register(&PeerInfo{ID: "peer-1", Height: 10})

	ctx, cancel := context.WithCancel(context.Background())

	go b.savePeerRegistryPeriodically(ctx, path, 50*time.Millisecond)

	// Wait for first save.
	require.Eventually(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)

	// Modify registry.
	b.peerRegistry.Register(&PeerInfo{ID: "peer-2", Height: 20})

	// Wait for another save to overwrite with the new data.
	require.Eventually(t, func() bool {
		r := NewCentralizedPeerRegistry(DefaultBanConfig())
		if err := r.Load(path, 24*time.Hour); err != nil {
			return false
		}
		return r.Count() == 2
	}, 2*time.Second, 20*time.Millisecond, "second peer should appear after periodic save")

	cancel()
}

// ---------------------------------------------------------------------------
// Stop then reload verifies full persistence round-trip
// ---------------------------------------------------------------------------

func TestRegistryServer_Stop_ReloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.json")

	// First instance: register multiple peers, then stop.
	b1 := newMinimalBlockchainForRegistry(t, path, time.Minute)
	b1.peerRegistry.Register(&PeerInfo{ID: "peer-a", Height: 100, ClientName: "clientA"})
	b1.peerRegistry.Register(&PeerInfo{ID: "peer-b", Height: 200, ClientName: "clientB"})

	err := b1.Stop(context.Background())
	require.NoError(t, err)

	// Second instance: load and verify.
	b2 := newMinimalBlockchainForRegistry(t, path, time.Minute)
	require.NoError(t, b2.peerRegistry.Load(path, 24*time.Hour))
	require.Equal(t, 2, b2.peerRegistry.Count())

	a, ok := b2.peerRegistry.Get("peer-a")
	require.True(t, ok)
	require.Equal(t, uint32(100), a.Height)
	require.Equal(t, "clientA", a.ClientName)

	b, ok := b2.peerRegistry.Get("peer-b")
	require.True(t, ok)
	require.Equal(t, uint32(200), b.Height)
	require.Equal(t, "clientB", b.ClientName)
}
