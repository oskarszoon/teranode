// Package recoverreplayedtransactions provides the operator replay-recovery command.
package recoverreplayedtransactions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	astore "github.com/bsv-blockchain/teranode/stores/utxo/aerospike"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
)

type Options struct {
	Mode              string
	Manifest          string
	Journal           string
	RPC               string
	HistoryIndex      string
	HistoryStart      uint32
	HistoryEnd        uint32
	Maintenance       bool
	Reset             bool
	Timeout           time.Duration
	RequestsPerSecond float64
	Concurrency       int
}

func (o Options) validate() error {
	switch o.Mode {
	case "discover", "apply", "resume", "verify", "export":
	default:
		return commandError("mode must be discover, apply, resume, verify, or export")
	}
	if o.Manifest == "" {
		return commandError("manifest path is required")
	}
	paths := map[string]bool{}
	for _, path := range []string{o.Manifest, o.Journal, o.HistoryIndex} {
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if paths[absolute] {
			return commandError("manifest, journal, and history index must have distinct paths")
		}
		paths[absolute] = true
	}
	if o.Mode == "export" {
		return nil
	}
	if o.Mode == "apply" || o.Mode == "resume" {
		if !o.Maintenance {
			return commandError("apply requires --maintenance acknowledgement that all shared-store writers are stopped")
		}
	}
	if o.Mode != "discover" && o.Journal == "" {
		return commandError("journal path is required")
	}
	if o.RPC == "" {
		return commandError("explicit trusted --rpc endpoint is required")
	}
	if o.Timeout <= 0 || o.RequestsPerSecond <= 0 || o.Concurrency < 1 || o.Concurrency > 64 {
		return commandError("timeout/rpc-rate must be positive and concurrency between 1 and 64")
	}
	if o.HistoryEnd != 0 && o.HistoryStart > o.HistoryEnd {
		return commandError("history-start exceeds history-end")
	}
	if o.HistoryIndex == "" && (o.HistoryStart != 0 || o.HistoryEnd != 0) {
		return commandError("history bounds require --history-index")
	}
	if o.Reset && o.Mode != "verify" {
		return commandError("--reset is valid only in verify mode")
	}
	return nil
}

// historyClient uses only read RPCs. Construction of a normal SQL blockchain
// store performs schema updates, so it is unsuitable for read-only discovery.
type historyClient struct{ blockchain.ClientI }

func (c historyClient) GetBlockInChainByHeightHash(ctx context.Context, height uint32, tip *chainhash.Hash) (*model.Block, bool, error) {
	header, _, err := c.GetBestBlockHeader(ctx)
	if err != nil {
		return nil, false, err
	}
	if header == nil || !header.Hash().IsEqual(tip) {
		return nil, false, commandError("local tip moved during historical lookup")
	}
	block, err := c.GetBlockByHeight(ctx, height)
	if err != nil {
		return nil, false, err
	}
	if block == nil || block.Height != height {
		return nil, false, commandError("historical block height mismatch")
	}
	// LocalHistory validates the body commitment; RPCSource additionally verifies
	// the containing hash at this height against its independently agreed tip.
	return block, false, nil
}

func readOnlyArchive(logger ulogger.Logger, configured *url.URL) (blob.Store, error) {
	if configured == nil {
		return nil, commandError("subtree store URL is not configured")
	}
	u := *configured
	q := u.Query()
	q.Set("disableDAH", "true")
	q.Del("batch")
	u.RawQuery = q.Encode()
	if u.Scheme == "file" {
		path := u.Path
		if u.Host == "." {
			path = strings.TrimPrefix(path, "/")
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, commandError("archive root is not a directory")
		}
	}
	return blob.NewStore(logger, &u, options.WithHashPrefix(2))
}

// Run writes a JSON summary to out and progress to progress. It does not start
// node writers and never changes ordinary reset semantics.
func Run(ctx context.Context, logger ulogger.Logger, s *settings.Settings, o Options, out, progress io.Writer) (err error) {
	if o.Mode == "" {
		o.Mode = "discover"
	}
	if err = o.validate(); err != nil {
		return err
	}
	if o.Mode == "export" {
		return replayrecovery.ExportManifest(ctx, o.Manifest, out)
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if s == nil || s.UtxoStore.UtxoStore == nil || s.UtxoStore.UtxoStore.Scheme != "aerospike" {
		return commandError("replay recovery supports only the Aerospike UTXO backend")
	}
	u := s.UtxoStore.UtxoStore
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return commandError("UTXO store URL must name namespace and set")
	}
	client, err := util.GetAerospikeClient(logger, u, s)
	if err != nil {
		return err
	}
	defer util.CloseAerospikeClient(u.Host)
	coreBackend, err := astore.NewRecoveryBackend(client.Client, parts[0], parts[1], s.UtxoStore.UtxoBatchSize, u.Host)
	if err != nil {
		return err
	}
	chain, err := blockchain.NewClient(ctx, logger, s, "replay-recovery")
	if err != nil {
		return err
	}
	if closer, ok := chain.(io.Closer); ok {
		defer func() { err = combineErrors(err, closer.Close()) }()
	}
	localHeader, localMeta, err := chain.GetBestBlockHeader(ctx)
	if err != nil {
		return err
	}
	if localHeader == nil || localMeta == nil {
		return commandError("local canonical tip unavailable")
	}
	localTip := replayrecovery.Tip{Hash: localHeader.Hash().String(), Height: localMeta.Height}
	var history *LocalHistory
	if o.HistoryIndex != "" {
		_, statErr := os.Lstat(o.HistoryIndex)
		if statErr == nil {
			history, err = OpenHistory(o.HistoryIndex)
		} else if errors.Is(statErr, os.ErrNotExist) && o.Mode == "discover" {
			archive, e := readOnlyArchive(logger, s.SubtreeValidation.SubtreeStore)
			if e != nil {
				return e
			}
			defer func() { err = combineErrors(err, archive.Close(ctx)) }()
			end := o.HistoryEnd
			if end == 0 {
				end = localTip.Height
			}
			lastHistoryProgress := time.Time{}
			history, err = NewHistory(ctx, o.HistoryIndex, historyClient{chain}, archive, HistoryOptions{StartHeight: o.HistoryStart, EndHeight: end, Progress: func(c HistoryCoverage) {
				if time.Since(lastHistoryProgress) >= 5*time.Second {
					_, _ = fmt.Fprintf(progress, "History scanned: %d blocks, %d gaps\n", c.Scanned, c.GapCount)
					lastHistoryProgress = time.Now()
				}
			}})
			if err == nil {
				_, _ = fmt.Fprintf(progress, "Indexing retained canonical history %d..%d at %s\n", o.HistoryStart, end, localTip.Hash)
				err = history.Build(ctx, localTip)
			}
		} else {
			err = statErr
		}
		if history != nil {
			defer func() { err = combineErrors(err, history.Close()) }()
		}
		if err != nil {
			return err
		}
		coverage, _ := json.Marshal(history.Coverage())
		_, _ = fmt.Fprintf(progress, "History coverage: %s\n", coverage)
	}
	var fallback replayrecovery.History
	if history != nil {
		fallback = history
	}
	source, err := replayrecovery.NewRPCSource(o.RPC, &http.Client{Timeout: 30 * time.Second}, fallback)
	if err != nil {
		return err
	}
	if err = source.Configure(replayrecovery.RPCOptions{MaxResponseBytes: 16 << 20, RequestsPerSecond: o.RequestsPerSecond, Concurrency: o.Concurrency, Timeout: 30 * time.Second}); err != nil {
		return err
	}
	tip, err := source.Tip(ctx)
	if err != nil {
		return err
	}
	if tip != localTip {
		return commandError("local canonical tip and trusted RPC disagree")
	}
	retained, closeRetained, err := openExternalTransactionReader(ctx, logger, u)
	if err != nil {
		return err
	}
	defer func() { err = combineErrors(err, closeRetained()) }()
	backend := replayrecovery.NewEvidenceBackend(coreBackend, source, localTip, retained)
	var summary replayrecovery.Summary
	switch o.Mode {
	case "apply", "resume":
		summary, err = replayrecovery.Apply(ctx, backend, source, o.Manifest, o.Journal, replayrecovery.ApplyOptions{Maintenance: o.Maintenance, Resume: o.Mode == "resume", Tip: localTip})
	case "discover", "verify":
		ba, e := blockassembly.NewClient(ctx, logger, s)
		if e != nil {
			return e
		}
		defer func() { err = combineErrors(err, ba.Close()) }()
		assembly := NewAssembly(ba)
		if o.Mode == "discover" {
			last := time.Time{}
			summary, err = replayrecovery.Discover(ctx, backend, source, assembly, o.Manifest, func(p replayrecovery.Summary) {
				if time.Since(last) >= 5*time.Second {
					_, _ = fmt.Fprintf(progress, "%s: %d transactions\n", p.Stage, p.Scanned)
					last = time.Now()
				}
			})
		} else {
			summary, err = replayrecovery.Verify(ctx, backend, source, assembly, o.Manifest, o.Journal, o.Reset, s.ChainCfgParams.GenesisActivationHeight)
		}
	}
	return combineErrors(err, json.NewEncoder(out).Encode(summary))
}

// ExitCode distinguishes an incomplete audit, pending verification, and failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, replayrecovery.ErrPending) {
		return 3
	}
	if errors.Is(err, replayrecovery.ErrIncomplete) {
		return 2
	}
	return 1
}
