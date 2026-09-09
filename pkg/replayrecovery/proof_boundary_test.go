package replayrecovery

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func boundaryMerkleProof(total uint32, root chainhash.Hash, hashes []chainhash.Hash, flags []byte) []byte {
	header := make([]byte, 80)
	copy(header[36:68], root[:])
	result := binary.LittleEndian.AppendUint32(header, total)
	result = append(result, byte(len(hashes)))
	for _, hash := range hashes {
		result = append(result, hash[:]...)
	}
	result = append(result, byte(len(flags)))
	return append(result, flags...)
}

func TestProofBoundaryRejectsAmbiguousPartialTrees(t *testing.T) {
	target := chainhash.DoubleHashH([]byte("confirmed transaction"))
	tests := []struct {
		name   string
		proof  []byte
		reason string
	}{
		{"zero transactions", boundaryMerkleProof(0, target, []chainhash.Hash{target}, []byte{1}), "invalid transaction count"},
		{"zero hashes", boundaryMerkleProof(1, target, nil, []byte{1}), "invalid proof hash count"},
		{"hashes exceed transactions", boundaryMerkleProof(1, target, []chainhash.Hash{target, target}, []byte{1}), "invalid proof hash count"},
		{"flags exhausted before leaf", boundaryMerkleProof(256, target, []chainhash.Hash{target}, []byte{255}), "proof flags exhausted"},
		{"missing right branch hash", boundaryMerkleProof(2, target, []chainhash.Hash{target}, []byte{7}), "proof hashes exhausted"},
		{"duplicated real leaves", boundaryMerkleProof(2, hashPair(target, target), []chainhash.Hash{target, target}, []byte{7}), "mutated merkle tree"},
		{"nonzero unused flags", boundaryMerkleProof(1, target, []chainhash.Hash{target}, []byte{129}), "nonzero proof padding"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeProof(test.proof, target.String())
			require.ErrorContains(t, err, test.reason)
		})
	}
	// The same leaf and header are admissible when the serialized tree is complete.
	inclusion, err := decodeProof(boundaryMerkleProof(1, target, []chainhash.Hash{target}, []byte{1}), target.String())
	require.NoError(t, err)
	_, err = VerifyInclusion(target.String(), inclusion)
	require.NoError(t, err)
}

func TestProofBoundaryRejectsNoncanonicalWireCounts(t *testing.T) {
	target := chainhash.DoubleHashH([]byte("wire count target"))
	original := boundaryMerkleProof(1, target, []chainhash.Hash{target}, []byte{1})
	counts := [][]byte{
		{253, 1, 0}, // A small count must use its one-byte encoding.
		{254, 1, 0, 0, 0},
		{255, 1, 0, 0, 0, 0, 0, 0, 0},
		{253, 253, 0}, // Canonical counts still cannot claim absent hashes.
		{254, 0, 0, 1, 0},
		{255, 0, 0, 0, 0, 1, 0, 0, 0},
	}
	for _, count := range counts {
		t.Run(hex.EncodeToString(count), func(t *testing.T) {
			proof := append([]byte(nil), original[:84]...)
			proof = append(proof, count...)
			proof = append(proof, original[85:]...)
			_, err := decodeProof(proof, target.String())
			require.ErrorContains(t, err, "invalid proof hash count")
		})
	}
}

func TestProofBoundaryValidatesIdentityAndBranchShape(t *testing.T) {
	target := chainhash.DoubleHashH([]byte("branch target"))
	header := make([]byte, 80)
	copy(header[36:68], target[:])
	for _, id := range []string{"short", strings.Repeat("gg", 32), strings.ToUpper(target.String())} {
		_, err := VerifyInclusion(id, Inclusion{Header: header})
		require.Error(t, err)
		_, err = decodeProof(boundaryMerkleProof(1, target, []chainhash.Hash{target}, []byte{1}), id)
		require.Error(t, err)
	}
	for _, inclusion := range []Inclusion{
		{Header: header[:79]},
		{Header: header, MerkleBranch: make([]string, 33)},
		{Header: header, MerkleBranch: []string{"not a sibling hash"}},
		{Header: header, Index: 1}, // A leaf-only proof cannot claim another position.
	} {
		_, err := VerifyInclusion(target.String(), inclusion)
		require.Error(t, err)
	}
}

func TestProofBoundaryRejectsTruncatedTransactions(t *testing.T) {
	raw, err := hex.DecodeString("0100000001" + strings.Repeat("00", 32) + "ffffffff0100ffffffff010100000000000000015100000000")
	require.NoError(t, err)
	for length := 0; length < len(raw); length++ {
		tx, err := ReadBoundedTransaction(bytes.NewReader(raw[:length]))
		require.Error(t, err, "accepted truncation at byte %d", length)
		require.Nil(t, tx)
	}
	// Oversized unlocking-script length, bad extended marker, and a truncated
	// extended input count must fail before any raw transaction can authorize work.
	oversized := append([]byte(nil), raw...)
	oversized[41] = byte(len(raw))
	invalidExtended := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	missingExtendedCount := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 239}
	for _, data := range [][]byte{oversized, invalidExtended, missingExtendedCount} {
		tx, err := ReadBoundedTransaction(bytes.NewReader(data))
		require.Error(t, err)
		require.Nil(t, tx)
	}
	tx, err := ReadBoundedTransaction(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, raw, tx.Bytes())
}
