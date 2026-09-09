package recoverreplayedtransactions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// HistoryOptions bounds each archive read and block's in-memory hash list.
// Zero limits default to 64 MiB per blob and 1,000,000 transactions per block.
// An archive exceeding a bound is a reported gap, never evidence of absence.
type HistoryOptions struct {
	// Progress runs synchronously after each scanned block; it must not call LocalHistory methods.
	Progress               func(HistoryCoverage) `json:"-"`
	StartHeight, EndHeight uint32
	MaxBlobBytes           int64
	MaxBlockTransactions   int
}
type HistoryCoverage struct {
	StartHeight uint32             `json:"start_height"`
	EndHeight   uint32             `json:"end_height"`
	Scanned     uint64             `json:"scanned"`
	GapCount    uint32             `json:"gap_count"`
	Complete    bool               `json:"complete"`
	Tip         replayrecovery.Tip `json:"tip"`
}

// HistoryChain exposes only canonical reads; discovery must not construct a
// production store that starts schema updates or background writers.
type HistoryChain interface {
	GetBestBlockHeader(context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error)
	GetBlockInChainByHeightHash(context.Context, uint32, *chainhash.Hash) (*model.Block, bool, error)
}

// LocalHistory scans the configured canonical range once. All retained raw
// transactions are indexed on disk, so newly discovered dependencies do not
// cause repeated archive scans. A fresh index is required for a different tip.
type LocalHistory struct {
	db       *sql.DB
	file     *os.File
	chain    HistoryChain
	archive  blob.Store
	options  HistoryOptions
	mu       sync.RWMutex
	coverage HistoryCoverage
	built    bool
}

func NewHistory(ctx context.Context, path string, chain HistoryChain, archive blob.Store, o HistoryOptions) (_ *LocalHistory, err error) {
	if o.MaxBlobBytes == 0 {
		o.MaxBlobBytes = 64 << 20
	}
	if o.MaxBlockTransactions == 0 {
		o.MaxBlockTransactions = 1000000
	}
	if chain == nil || archive == nil || o.EndHeight < o.StartHeight || o.MaxBlobBytes < 64 || o.MaxBlobBytes > 256<<20 || o.MaxBlockTransactions < 1 || o.MaxBlockTransactions > 4000000 {
		return nil, errors.NewProcessingError("invalid history configuration")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, errors.NewProcessingError("history directory must be private (0700)")
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err != nil {
			_ = file.Close()
		}
	}()
	if err = file.Close(); err != nil {
		return nil, err
	}
	lockFD, lockErr := unix.Open(path+".lock", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if lockErr != nil {
		return nil, lockErr
	}
	file = os.NewFile(uintptr(lockFD), path+".lock")
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL; PRAGMA cache_size=-4096;
 CREATE TABLE blocks(hash TEXT PRIMARY KEY,height INTEGER NOT NULL,header BLOB NOT NULL,leaves BLOB NOT NULL);
 CREATE TABLE transactions(txid TEXT NOT NULL,block TEXT NOT NULL,position INTEGER NOT NULL,raw BLOB NOT NULL,PRIMARY KEY(txid,block));
 CREATE TABLE gaps(height INTEGER PRIMARY KEY,reason TEXT NOT NULL);
 CREATE TABLE coverage(data BLOB NOT NULL);`)
	if err != nil {
		return nil, err
	}
	return &LocalHistory{db: db, file: file, chain: chain, archive: archive, options: o, coverage: HistoryCoverage{StartHeight: o.StartHeight, EndHeight: o.EndHeight}}, nil
}
func (h *LocalHistory) Close() error {
	dbErr, lockErr := h.db.Close(), h.file.Close()
	if dbErr == nil {
		return lockErr
	}
	if lockErr == nil {
		return dbErr
	}
	return errors.Join(dbErr, lockErr)
}

func (h *LocalHistory) Coverage() HistoryCoverage {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.coverage
}
func (h *LocalHistory) Gaps(ctx context.Context, visit func(uint32, string) error) error {
	rows, err := h.db.QueryContext(ctx, "SELECT height,reason FROM gaps ORDER BY height")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var height uint32
		var reason string
		if err = rows.Scan(&height, &reason); err != nil {
			return err
		}
		if err = visit(height, reason); err != nil {
			return err
		}
	}
	return rows.Err()
}
func (h *LocalHistory) checkTip(ctx context.Context, tip replayrecovery.Tip) error {
	header, meta, err := h.chain.GetBestBlockHeader(ctx)
	if err != nil {
		return err
	}
	if header == nil || meta == nil || header.Hash().String() != tip.Hash || meta.Height != tip.Height {
		return errors.NewProcessingError("local canonical tip changed or disagrees")
	}
	return nil
}
func (h *LocalHistory) Build(ctx context.Context, tip replayrecovery.Tip) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.built {
		if h.coverage.Tip != tip {
			return errors.NewProcessingError("history index bound to different tip")
		}
		return h.checkTip(ctx, tip)
	}
	if h.coverage.Scanned != 0 {
		return errors.NewProcessingError("interrupted history build requires fresh index")
	}
	if h.options.EndHeight > tip.Height {
		return errors.NewProcessingError("history range extends beyond tip")
	}
	if err := h.checkTip(ctx, tip); err != nil {
		return err
	}
	tipHash, err := chainhash.NewHashFromStr(tip.Hash)
	if err != nil {
		return err
	}
	h.coverage.Tip = tip
	for height := uint64(h.options.StartHeight); height <= uint64(h.options.EndHeight); height++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		block, invalid, readErr := h.chain.GetBlockInChainByHeightHash(ctx, uint32(height), tipHash) // #nosec G115 -- loop height <= EndHeight, whose type is uint32.
		if readErr == nil && (invalid || block == nil) {
			readErr = errors.NewProcessingError("canonical block unavailable")
		}
		if readErr == nil {
			readErr = h.indexBlock(ctx, block, uint32(height)) // #nosec G115 -- loop height <= EndHeight, whose type is uint32.
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readErr != nil {
			if _, err = h.db.ExecContext(ctx, "INSERT INTO gaps(height,reason) VALUES(?,?)", height, readErr.Error()); err != nil {
				return err
			}
			h.coverage.GapCount++
		}
		h.coverage.Scanned++
		if h.options.Progress != nil {
			h.options.Progress(h.coverage)
		}
	}
	if err = h.checkTip(ctx, tip); err != nil {
		return err
	}
	h.coverage.Complete = h.coverage.GapCount == 0
	data, err := json.Marshal(historySeal{Version: 1, Options: h.options, Coverage: h.coverage})
	if err != nil {
		return err
	}
	if _, err = h.db.ExecContext(ctx, "INSERT INTO coverage(data) VALUES(?)", data); err != nil {
		return err
	}
	h.built = true
	return nil
}
func (h *LocalHistory) readBlob(ctx context.Context, key []byte, kind fileformat.FileType) ([]byte, error) {
	r, err := h.archive.GetIoReader(ctx, key, kind)
	if err != nil {
		return nil, errors.NewProcessingError("archive %s unavailable", kind)
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, h.options.MaxBlobBytes+1))
	if err != nil {
		return nil, errors.NewProcessingError("archive read failed")
	}
	if int64(len(b)) > h.options.MaxBlobBytes {
		return nil, errors.NewProcessingError("archive exceeds byte limit")
	}
	return b, nil
}
func historyPair(a, b chainhash.Hash) chainhash.Hash {
	var p [64]byte
	copy(p[:32], a[:])
	copy(p[32:], b[:])
	return chainhash.DoubleHashH(p[:])
}
func historyRoot(leaves []chainhash.Hash) (chainhash.Hash, error) {
	if len(leaves) == 0 {
		return chainhash.Hash{}, errors.NewProcessingError("empty merkle tree")
	}
	level := append([]chainhash.Hash(nil), leaves...)
	for len(level) > 1 {
		next := make([]chainhash.Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			right := level[i]
			if i+1 < len(level) {
				right = level[i+1]
				if right == level[i] {
					return chainhash.Hash{}, errors.NewProcessingError("mutated merkle tree")
				}
			}
			next = append(next, historyPair(level[i], right))
		}
		level = next
	}
	return level[0], nil
}

// subtreeLeaves parses the fixed-width archive without trusting allocation counts
// or its cached root hash. Fees/size metadata do not authenticate membership.
func (h *LocalHistory) subtreeLeaves(data []byte, key *chainhash.Hash) ([]chainhash.Hash, error) {
	if len(data) < 64 || !bytes.Equal(data[:32], key[:]) {
		return nil, errors.NewProcessingError("subtree header mismatch")
	}
	count := binary.LittleEndian.Uint64(data[48:56])
	if count == 0 || count > uint64(h.options.MaxBlockTransactions) || count > uint64((len(data)-64)/48) { // #nosec G115 -- constructor validates positive MaxBlockTransactions; len(data) >= 64 above.
		return nil, errors.NewProcessingError("subtree leaf count exceeds bounds")
	}
	end := 56 + int(count)*48 // #nosec G115 -- count <= (len(data)-64)/48 above, so offset arithmetic fits int.
	conflicts := binary.LittleEndian.Uint64(data[end : end+8])
	if conflicts > uint64((len(data)-end-8)/32) || uint64(len(data)-end-8) != conflicts*32 { // #nosec G115 -- bounded count guarantees end+8 <= len(data); remainder is nonnegative.
		return nil, errors.NewProcessingError("malformed subtree conflicts")
	}
	leaves := make([]chainhash.Hash, int(count)) // #nosec G115 -- count <= (len(data)-64)/48, which fits int.
	for i := range leaves {
		copy(leaves[i][:], data[56+i*48:88+i*48])
	}
	root, err := historyRoot(leaves)
	if err != nil {
		return nil, err
	}
	if root != *key {
		return nil, errors.NewProcessingError("subtree root mismatch")
	}
	return leaves, nil
}
func (h *LocalHistory) indexBlock(ctx context.Context, b *model.Block, height uint32) (err error) {
	if b.Header == nil || b.Header.HashPrevBlock == nil || b.Header.HashMerkleRoot == nil || b.CoinbaseTx == nil || !b.CoinbaseTx.IsCoinbase() || b.TransactionCount == 0 || b.TransactionCount > uint64(h.options.MaxBlockTransactions) { // #nosec G115 -- constructor validates MaxBlockTransactions in [1,4000000].
		return errors.NewProcessingError("block metadata missing or exceeds transaction bound")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	block := b.Hash().String()
	leaves := make([]chainhash.Hash, 0, int(b.TransactionCount)) // #nosec G115 -- transaction count is bounded by MaxBlockTransactions <= 4000000 above.
	insert := func(transaction *bt.Tx) error {
		_, e := tx.ExecContext(ctx, "INSERT INTO transactions(txid,block,position,raw) VALUES(?,?,?,?)", transaction.TxID(), block, len(leaves), transaction.Bytes())
		return e
	}
	if len(b.Subtrees) == 0 {
		if b.TransactionCount != 1 {
			return errors.NewProcessingError("missing subtrees")
		}
		if err = insert(b.CoinbaseTx); err != nil {
			return err
		}
		leaves = append(leaves, *b.CoinbaseTx.TxIDChainHash())
	}
	for subtreeIndex, key := range b.Subtrees {
		if err = ctx.Err(); err != nil {
			return err
		}
		if key == nil {
			return errors.NewProcessingError("nil subtree reference")
		}
		data, e := h.readBlob(ctx, key[:], fileformat.FileTypeSubtree)
		if e != nil {
			return e
		}
		nodes, e := h.subtreeLeaves(data, key)
		if e != nil {
			return e
		}
		if len(leaves)+len(nodes) > h.options.MaxBlockTransactions {
			return errors.NewProcessingError("block exceeds transaction bound")
		}
		start := 0
		if subtreeIndex == 0 {
			if nodes[0] != *subtree.CoinbasePlaceholderHash {
				return errors.NewProcessingError("missing coinbase placeholder")
			}
			if err = insert(b.CoinbaseTx); err != nil {
				return err
			}
			leaves = append(leaves, *b.CoinbaseTx.TxIDChainHash())
			start = 1
		}
		if start == len(nodes) {
			continue
		}
		raw, e := h.readBlob(ctx, key[:], fileformat.FileTypeSubtreeData)
		if e != nil {
			return e
		}
		reader := bytes.NewReader(raw)
		for i := start; i < len(nodes); i++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			transaction, parseErr := replayrecovery.ReadBoundedTransaction(reader)
			if parseErr != nil {
				return errors.NewProcessingError("archived transaction missing or malformed")
			}
			if *transaction.TxIDChainHash() != nodes[i] {
				return errors.NewProcessingError("archived transaction identity mismatch")
			}
			if err = insert(transaction); err != nil {
				return err
			}
			leaves = append(leaves, nodes[i])
		}
		if reader.Len() != 0 {
			return errors.NewProcessingError("trailing archive transaction bytes")
		}
	}
	if uint64(len(leaves)) != b.TransactionCount {
		return errors.NewProcessingError("block transaction count mismatch")
	}
	root, err := historyRoot(leaves)
	if err != nil {
		return err
	}
	if root != *b.Header.HashMerkleRoot {
		return errors.NewProcessingError("block merkle commitment mismatch")
	}
	hashes := make([]byte, 0, len(leaves)*32)
	for _, hash := range leaves {
		hashes = append(hashes, hash[:]...)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO blocks(hash,height,header,leaves) VALUES(?,?,?,?)", block, height, b.Header.Bytes(), hashes); err != nil {
		return err
	}
	return tx.Commit()
}
func (h *LocalHistory) Lookup(ctx context.Context, id string, tip replayrecovery.Tip) (replayrecovery.Inclusion, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	p := replayrecovery.Inclusion{}
	if !h.built {
		return p, errors.NewProcessingError("history index unavailable for tip")
	}
	if h.chain != nil {
		if err := h.checkTip(ctx, tip); err != nil {
			return p, err
		}
	}
	var raw, leaves []byte
	err := h.db.QueryRowContext(ctx, `SELECT t.raw,t.position,b.header,b.height,b.leaves FROM transactions t JOIN blocks b ON t.block=b.hash WHERE t.txid=? AND length(t.raw)<=? AND length(b.header)=80 AND length(b.leaves) BETWEEN 32 AND ? ORDER BY b.height DESC LIMIT 1`, id, h.options.MaxBlobBytes, h.options.MaxBlockTransactions*32).Scan(&raw, &p.Index, &p.Header, &p.Height, &leaves)
	if err != nil {
		return p, errors.NewProcessingError("transaction history unavailable in range %d..%d (%d gaps)", h.coverage.StartHeight, h.coverage.EndHeight, h.coverage.GapCount)
	}
	if p.Height > tip.Height {
		return p, errors.NewProcessingError("historical inclusion beyond requested tip")
	}
	p.RawTx = hex.EncodeToString(raw)
	if len(leaves)%32 != 0 || len(leaves)/32 > h.options.MaxBlockTransactions || int(p.Index) >= len(leaves)/32 {
		return p, errors.NewProcessingError("corrupt history index")
	}
	level := make([]chainhash.Hash, len(leaves)/32)
	for i := range level {
		copy(level[i][:], leaves[i*32:(i+1)*32])
	}
	index := int(p.Index)
	for len(level) > 1 {
		other := index ^ 1
		if other >= len(level) {
			other = index
		}
		p.MerkleBranch = append(p.MerkleBranch, level[other].String())
		next := make([]chainhash.Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			r := i + 1
			if r == len(level) {
				r = i
			}
			next = append(next, historyPair(level[i], level[r]))
		}
		level = next
		index /= 2
	}
	if _, err = replayrecovery.VerifyInclusion(id, p); err != nil {
		return p, err
	}
	return p, nil
}

type historySeal struct {
	Version  int             `json:"version"`
	Options  HistoryOptions  `json:"options"`
	Coverage HistoryCoverage `json:"coverage"`
}

// OpenHistory opens an immutable completed index. Historical proofs remain valid
// across tips; RPCSource must recheck canonical block membership at the new tip.
func OpenHistory(path string) (_ *LocalHistory, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, errors.NewProcessingError("history directory must be private (0700)")
	}
	for _, p := range []string{path, path + ".lock"} {
		st, e := os.Lstat(p)
		if e != nil {
			return nil, e
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return nil, errors.NewProcessingError("history files must be private regular files")
		}
	}
	fd, err := unix.Open(path+".lock", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path+".lock")
	defer func() {
		if err != nil {
			_ = file.Close()
		}
	}()
	if err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return nil, errors.NewProcessingError("history index is being built", err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	var data []byte
	if err = db.QueryRow("SELECT data FROM coverage WHERE length(data)<=65536").Scan(&data); err != nil {
		return nil, errors.NewProcessingError("history index incomplete", err)
	}
	var seal historySeal
	if err = json.Unmarshal(data, &seal); err != nil {
		return nil, err
	}
	o := seal.Options
	c := seal.Coverage
	if seal.Version != 1 || o.EndHeight < o.StartHeight || o.MaxBlockTransactions < 1 || o.MaxBlockTransactions > 4000000 || o.MaxBlobBytes < 64 || o.MaxBlobBytes > 256<<20 || c.StartHeight != o.StartHeight || c.EndHeight != o.EndHeight || c.Scanned != uint64(o.EndHeight)-uint64(o.StartHeight)+1 || uint64(c.GapCount) > c.Scanned || c.Complete != (c.GapCount == 0) {
		return nil, errors.NewProcessingError("invalid history index seal")
	}
	return &LocalHistory{db: db, file: file, options: o, coverage: c, built: true}, nil
}
