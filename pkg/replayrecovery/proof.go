package replayrecovery

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
)

// Inclusion contains independently checkable transaction inclusion evidence.
// Header is the exact 80-byte Bitcoin header; branches use display hash order.
type Inclusion struct {
	RawTx        string   `json:"raw_tx"`
	Header       []byte   `json:"header"`
	Height       uint32   `json:"height"`
	MerkleBranch []string `json:"merkle_branch"`
	Index        uint32   `json:"index"`
}

// VerifyInclusion authenticates the branch against the exact header bytes.
// Canonical chain membership must be established separately at the pinned tip.
func VerifyInclusion(id string, p Inclusion) (string, error) {
	h, err := strictHash(id)
	if err != nil {
		return "", err
	}
	if len(p.Header) != 80 || len(p.MerkleBranch) > 32 {
		return "", fmt.Errorf("invalid inclusion header or branch length")
	}
	index := p.Index
	for _, s := range p.MerkleBranch {
		other, e := strictHash(s)
		if e != nil {
			return "", e
		}
		if index&1 == 0 {
			*h = hashPair(*h, *other)
		} else {
			*h = hashPair(*other, *h)
		}
		index >>= 1
	}
	if index != 0 || !bytes.Equal(h[:], p.Header[36:68]) {
		return "", fmt.Errorf("merkle root mismatch")
	}
	return chainhash.DoubleHashH(p.Header).String(), nil
}

func strictHash(s string) (*chainhash.Hash, error) {
	if len(s) != 64 {
		return nil, fmt.Errorf("invalid hash length")
	}
	h, e := chainhash.NewHashFromStr(s)
	if e != nil || h.String() != s {
		return nil, fmt.Errorf("invalid canonical hash")
	}
	return h, nil
}
func hashPair(a, b chainhash.Hash) chainhash.Hash {
	var pair [64]byte
	copy(pair[:32], a[:])
	copy(pair[32:], b[:])
	return chainhash.DoubleHashH(pair[:])
}

func compact(r *bytes.Reader) (uint64, error) {
	b, e := r.ReadByte()
	if e != nil {
		return 0, e
	}
	var n uint64
	switch b {
	case 253:
		var x uint16
		e = binary.Read(r, binary.LittleEndian, &x)
		n = uint64(x)
		if n < 253 {
			return 0, fmt.Errorf("noncanonical varint")
		}
	case 254:
		var x uint32
		e = binary.Read(r, binary.LittleEndian, &x)
		n = uint64(x)
		if n <= 65535 {
			return 0, fmt.Errorf("noncanonical varint")
		}
	case 255:
		e = binary.Read(r, binary.LittleEndian, &n)
		if n <= 4294967295 {
			return 0, fmt.Errorf("noncanonical varint")
		}
	default:
		n = uint64(b)
	}
	return n, e
}

// decodeProof verifies Bitcoin's serialized partial Merkle tree without trusting
// gettxoutproof's reported matches. Allocations are bounded by the response size.
func decodeProof(data []byte, id string) (Inclusion, error) {
	p := Inclusion{}
	target, err := strictHash(id)
	if err != nil {
		return p, err
	}
	if len(data) < 86 {
		return p, fmt.Errorf("short merkle proof")
	}
	p.Header = append([]byte(nil), data[:80]...)
	r := bytes.NewReader(data[80:])
	var total uint32
	if err = binary.Read(r, binary.LittleEndian, &total); err != nil || total == 0 {
		return p, fmt.Errorf("invalid transaction count")
	}
	n, err := compact(r)
	if err != nil || n == 0 || n > uint64(total) || n > uint64(r.Len()/32) { // #nosec G115 -- bytes.Reader.Len is nonnegative.
		return p, fmt.Errorf("invalid proof hash count")
	}
	hashes := make([]chainhash.Hash, int(n)) // #nosec G115 -- n was bounded by r.Len()/32, which fits int.
	for i := range hashes {
		if _, err = io.ReadFull(r, hashes[i][:]); err != nil {
			return p, err
		}
	}
	count, err := compact(r)
	if err != nil || count == 0 || count > uint64(r.Len()) { // #nosec G115 -- bytes.Reader.Len is nonnegative.
		return p, fmt.Errorf("invalid proof flags")
	}
	flags := make([]byte, int(count)) // #nosec G115 -- count was bounded by r.Len(), which fits int.
	_, err = io.ReadFull(r, flags)
	if err != nil || r.Len() != 0 {
		return p, fmt.Errorf("trailing or truncated proof")
	}
	width := func(height uint32) uint64 { return (uint64(total) + (uint64(1) << height) - 1) >> height }
	height := uint32(0)
	for width(height) > 1 {
		height++
	}
	bits, used, matches := 0, 0, 0
	var walk func(uint32, uint32) (chainhash.Hash, bool, error)
	walk = func(h, pos uint32) (chainhash.Hash, bool, error) {
		if bits >= len(flags)*8 {
			return chainhash.Hash{}, false, fmt.Errorf("proof flags exhausted")
		}
		match := flags[bits/8]&(1<<uint(bits%8)) != 0
		bits++
		if h == 0 || !match {
			if used >= len(hashes) {
				return chainhash.Hash{}, false, fmt.Errorf("proof hashes exhausted")
			}
			v := hashes[used]
			used++
			found := h == 0 && match && v == *target
			if found {
				matches++
				p.Index = pos
			}
			return v, found, nil
		}
		left, lf, e := walk(h-1, pos*2)
		if e != nil {
			return left, false, e
		}
		right, rf := left, false
		if uint64(pos)*2+1 < width(h-1) {
			right, rf, e = walk(h-1, pos*2+1)
			if e != nil {
				return right, false, e
			}
			if left == right {
				return right, false, fmt.Errorf("mutated merkle tree")
			}
		}
		if lf {
			p.MerkleBranch = append(p.MerkleBranch, right.String())
		}
		if rf {
			p.MerkleBranch = append(p.MerkleBranch, left.String())
		}
		return hashPair(left, right), lf || rf, nil
	}
	root, _, err := walk(height, 0)
	if err != nil {
		return p, err
	}
	if matches != 1 || used != len(hashes) || (bits+7)/8 != len(flags) || !bytes.Equal(root[:], p.Header[36:68]) {
		return p, fmt.Errorf("invalid merkle proof")
	}
	for bit := bits; bit < len(flags)*8; bit++ {
		if flags[bit/8]&(1<<uint(bit%8)) != 0 {
			return p, fmt.Errorf("nonzero proof padding")
		}
	}
	return p, nil
}

// ReadBoundedTransaction validates wire counts and script lengths against bytes
// actually present before bt allocates scripts. It accepts standard or extended
// archive encoding and consumes exactly one transaction from the reader.
func ReadBoundedTransaction(r *bytes.Reader) (*bt.Tx, error) {
	start := r.Size() - int64(r.Len())
	skip := func(n uint64) error {
		if n > uint64(r.Len()) { // #nosec G115 -- bytes.Reader.Len is nonnegative.
			return fmt.Errorf("transaction field exceeds available bytes")
		}
		_, err := r.Seek(int64(n), io.SeekCurrent) // #nosec G115 -- n <= r.Len() <= MaxInt, therefore fits int64.
		return err
	}
	script := func() error {
		n, err := compact(r)
		if err != nil {
			return err
		}
		return skip(n)
	}
	if err := skip(4); err != nil {
		return nil, err
	}
	inputs, err := compact(r)
	if err != nil {
		return nil, err
	}
	extended := false
	if inputs == 0 {
		marker := make([]byte, 5)
		if _, err = io.ReadFull(r, marker); err != nil || !bytes.Equal(marker, []byte{0, 0, 0, 0, 239}) {
			return nil, fmt.Errorf("invalid transaction input count")
		}
		extended = true
		inputs, err = compact(r)
		if err != nil {
			return nil, err
		}
	}
	if inputs == 0 || inputs > uint64(r.Len()/41) { // #nosec G115 -- bytes.Reader.Len is nonnegative.
		return nil, fmt.Errorf("transaction input count exceeds available bytes")
	}
	for i := uint64(0); i < inputs; i++ {
		if err = skip(36); err != nil {
			return nil, err
		}
		if err = script(); err != nil {
			return nil, err
		}
		if err = skip(4); err != nil {
			return nil, err
		}
		if extended {
			if err = skip(8); err != nil {
				return nil, err
			}
			if err = script(); err != nil {
				return nil, err
			}
		}
	}
	outputs, err := compact(r)
	if err != nil {
		return nil, err
	}
	if outputs == 0 || outputs > uint64(r.Len()/9) { // #nosec G115 -- bytes.Reader.Len is nonnegative.
		return nil, fmt.Errorf("transaction output count exceeds available bytes")
	}
	for i := uint64(0); i < outputs; i++ {
		if err = skip(8); err != nil {
			return nil, err
		}
		if err = script(); err != nil {
			return nil, err
		}
	}
	if err = skip(4); err != nil {
		return nil, err
	}
	end := r.Size() - int64(r.Len())
	if _, err = r.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	tx := &bt.Tx{}
	n, err := tx.ReadFrom(r)
	if err != nil {
		return nil, err
	}
	if n != end-start {
		return nil, fmt.Errorf("transaction encoding length mismatch")
	}
	return tx, nil
}
