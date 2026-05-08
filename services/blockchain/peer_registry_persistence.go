package blockchain

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
)

// savePeerRegistry marshals peers to JSON and atomically replaces the file at
// path. A temp file is written first and then renamed so readers never see a
// partial write.
func savePeerRegistry(path string, peers []*PeerInfo) error {
	data, err := json.Marshal(peers)
	if err != nil {
		return errors.NewProcessingError("marshal peer registry", err)
	}

	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return errors.NewProcessingError("write peer registry tmp file", err)
	}

	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return errors.NewProcessingError("rename peer registry tmp file", err)
	}

	return nil
}

// loadPeerRegistry reads and deserialises the peer registry from path, discarding
// entries whose LastSeen timestamp is older than ttl.
//
// Two non-fatal situations are handled silently:
//   - File does not exist: returns an empty slice (first startup is fine).
//   - File is corrupt: renamed to path+".corrupted" so operators can inspect it,
//     a warning is printed to stderr, and an empty slice is returned.
func loadPeerRegistry(path string, ttl time.Duration) ([]*PeerInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*PeerInfo{}, nil
		}
		return nil, errors.NewProcessingError("read peer registry", err)
	}

	var peers []*PeerInfo
	if err = json.Unmarshal(data, &peers); err != nil {
		corrupted := path + ".corrupted"
		// Best-effort rename — ignore error since we are already in a degraded path.
		_ = os.Rename(path, corrupted)
		fmt.Fprintf(os.Stderr, "peer registry: corrupt file renamed to %s, starting with empty registry: %v\n", corrupted, err)
		return []*PeerInfo{}, nil
	}

	cutoff := time.Now().Add(-ttl)
	live := make([]*PeerInfo, 0, len(peers))
	for _, p := range peers {
		if p.LastSeen.After(cutoff) {
			live = append(live, p)
		}
	}

	return live, nil
}

// Save persists the current registry state to path. Safe to call concurrently.
func (r *CentralizedPeerRegistry) Save(path string) error {
	r.mu.RLock()
	peers := make([]*PeerInfo, 0, len(r.peers))
	for _, p := range r.peers {
		peerCopy := *p
		peers = append(peers, &peerCopy)
	}
	r.mu.RUnlock()

	return savePeerRegistry(path, peers)
}

// Load reads the registry from path and replaces the current in-memory state.
// Stale entries (older than ttl) are dropped on load.
func (r *CentralizedPeerRegistry) Load(path string, ttl time.Duration) error {
	peers, err := loadPeerRegistry(path, ttl)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.peers = make(map[string]*PeerInfo, len(peers))
	for _, p := range peers {
		entry := *p
		r.peers[entry.ID] = &entry
	}

	return nil
}
