package blockchain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
)

// persistedRegistry is the on-disk JSON envelope. Versioned so format changes
// (e.g. adding more fields, splitting files) can be handled without losing the
// existing operator state.
type persistedRegistry struct {
	Version   int                          `json:"version"`
	SavedAt   time.Time                    `json:"saved_at"`
	Peers     []*PeerInfo                  `json:"peers"`
	BanScores map[string]persistedBanEntry `json:"ban_scores,omitempty"`
}

// persistedBanEntry mirrors the in-memory banEntry but is exported for JSON.
// We don't expose banEntry directly because it is intentionally package-private.
type persistedBanEntry struct {
	Score     int32     `json:"score"`
	Banned    bool      `json:"banned"`
	BanUntil  time.Time `json:"ban_until"`
	LastDecay time.Time `json:"last_decay"`
	Reasons   []string  `json:"reasons,omitempty"`
}

const persistedRegistryVersion = 1

// savePeerRegistry marshals the registry envelope to JSON and atomically
// replaces the file at path. A temp file is written first and then renamed so
// readers never see a partial write.
func savePeerRegistry(path string, peers []*PeerInfo, banScores map[string]persistedBanEntry) error {
	envelope := persistedRegistry{
		Version:   persistedRegistryVersion,
		SavedAt:   time.Now().UTC(),
		Peers:     peers,
		BanScores: banScores,
	}

	data, err := json.Marshal(&envelope)
	if err != nil {
		return errors.NewProcessingError("marshal peer registry", err)
	}

	// Use a unique temp file per call so concurrent saves (periodic ticker +
	// shutdown) don't clobber each other. os.CreateTemp gives us O_EXCL +
	// 0600 perms in one shot.
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return errors.NewProcessingError("create peer registry tmp file", err)
	}
	tmp := tmpFile.Name()

	if _, err = tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return errors.NewProcessingError("write peer registry tmp file", err)
	}
	if err = tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return errors.NewProcessingError("close peer registry tmp file", err)
	}

	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return errors.NewProcessingError("rename peer registry tmp file", err)
	}

	return nil
}

// loadPeerRegistry reads and deserialises the peer registry from path,
// discarding peer entries whose LastSeen timestamp is older than ttl. Banned
// peers are exempt from the TTL filter — bans must outlive idle gaps,
// otherwise restarts would silently clear in-flight bans. Their associated
// ban-score map is returned alongside.
//
// Two non-fatal situations are handled silently:
//   - File does not exist: returns empty state (first startup is fine).
//   - File is corrupt: renamed to path+".corrupted" so operators can inspect
//     it, a warning is printed to stderr, and empty state is returned.
func loadPeerRegistry(path string, ttl time.Duration) ([]*PeerInfo, map[string]persistedBanEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*PeerInfo{}, nil, nil
		}
		return nil, nil, errors.NewProcessingError("read peer registry", err)
	}

	var envelope persistedRegistry
	if err = json.Unmarshal(data, &envelope); err != nil {
		corrupted := path + ".corrupted"
		// Best-effort rename — ignore error since we are already in a degraded path.
		_ = os.Rename(path, corrupted)
		fmt.Fprintf(os.Stderr, "peer registry: corrupt file renamed to %s, starting with empty registry: %v\n", corrupted, err)
		return []*PeerInfo{}, nil, nil
	}

	cutoff := time.Now().Add(-ttl)
	live := make([]*PeerInfo, 0, len(envelope.Peers))
	for _, p := range envelope.Peers {
		// Banned peers are always retained: dropping them on TTL would let a
		// peer clear its own ban by going quiet for ttl, then reconnecting.
		if p.IsBanned || p.LastSeen.After(cutoff) {
			live = append(live, p)
		}
	}

	return live, envelope.BanScores, nil
}

// Save persists the current registry state to path. Safe to call concurrently
// — saveMu serializes the snapshot+write+rename so a slow earlier save can't
// rename its older snapshot over a newer one.
func (r *CentralizedPeerRegistry) Save(path string) error {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()

	r.mu.RLock()
	peers := make([]*PeerInfo, 0, len(r.peers))
	for _, p := range r.peers {
		peerCopy := *p
		peers = append(peers, &peerCopy)
	}
	bans := make(map[string]persistedBanEntry, len(r.banScores))
	for id, entry := range r.banScores {
		reasonsCopy := append([]string(nil), entry.Reasons...)
		bans[id] = persistedBanEntry{
			Score:     entry.Score,
			Banned:    entry.Banned,
			BanUntil:  entry.BanUntil,
			LastDecay: entry.LastDecay,
			Reasons:   reasonsCopy,
		}
	}
	r.mu.RUnlock()

	return savePeerRegistry(path, peers, bans)
}

// Load reads the registry from path and replaces the current in-memory state.
// Stale peer entries (older than ttl, not banned) are dropped on load.
// Ban-score entries that have already expired (BanUntil in the past) are
// discarded; everything else is restored so a node restart does not reset
// in-flight bans.
func (r *CentralizedPeerRegistry) Load(path string, ttl time.Duration) error {
	peers, bans, err := loadPeerRegistry(path, ttl)
	if err != nil {
		return err
	}

	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.peers = make(map[string]*PeerInfo, len(peers))
	for _, p := range peers {
		entry := *p
		r.peers[entry.ID] = &entry
	}

	r.banScores = make(map[string]*banEntry, len(bans))
	for id, b := range bans {
		// Drop ban entries whose window already closed before this load.
		if b.Banned && now.After(b.BanUntil) {
			continue
		}
		// Anchor LastDecay if missing so the next AddBanScore call doesn't
		// retroactively decay across the entire restart gap.
		lastDecay := b.LastDecay
		if lastDecay.IsZero() {
			lastDecay = now
		}
		r.banScores[id] = &banEntry{
			Score:     b.Score,
			Banned:    b.Banned,
			BanUntil:  b.BanUntil,
			LastDecay: lastDecay,
			Reasons:   append([]string(nil), b.Reasons...),
		}
	}

	// Reconcile PeerInfo.IsBanned / BanScore with the surviving banScores
	// map. A peer can carry IsBanned=true on disk while its corresponding
	// ban entry has just expired (and therefore got dropped above);
	// without this sync, selection and cleanup paths would treat the peer
	// as banned even though IsBannedPeer() returns false.
	for id, p := range r.peers {
		entry, ok := r.banScores[id]
		switch {
		case !ok:
			p.IsBanned = false
			p.BanScore = 0
		default:
			p.IsBanned = entry.Banned
			p.BanScore = entry.Score
		}
	}

	return nil
}
