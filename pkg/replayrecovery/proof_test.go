package replayrecovery

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func TestProofRejectsMalformedTrees(t *testing.T) {
	id := chainhash.DoubleHashH([]byte("tx"))
	header := make([]byte, 80)
	copy(header[36:68], id[:])
	valid := append([]byte(nil), header...)
	valid = binary.LittleEndian.AppendUint32(valid, 1)
	valid = append(valid, 1)
	valid = append(valid, id[:]...)
	valid = append(valid, 1, 1)
	inc, err := decodeProof(valid, id.String())
	require.NoError(t, err)
	require.Empty(t, inc.MerkleBranch)
	for _, p := range [][]byte{nil, valid[:len(valid)-1], append(append([]byte(nil), valid...), 0)} {
		_, err = decodeProof(p, id.String())
		require.Error(t, err)
	}
	invalid := append([]byte(nil), valid...)
	invalid[len(invalid)-1] = 0
	_, err = decodeProof(invalid, id.String())
	require.Error(t, err)
}

func TestProofTransactionBounds(t *testing.T) {
	data := make([]byte, 0, 50)
	data = append(data, 1, 0, 0, 0, 1)
	data = append(data, make([]byte, 36)...)
	data = append(data, 255, 255, 255, 255, 255, 255, 255, 255, 127)
	_, err := ReadBoundedTransaction(bytes.NewReader(data))
	require.Error(t, err)
}

func TestProofOddLeafBranch(t *testing.T) {
	a, b, c := chainhash.DoubleHashH([]byte("a")), chainhash.DoubleHashH([]byte("b")), chainhash.DoubleHashH([]byte("c"))
	left := hashPair(a, b)
	root := hashPair(left, hashPair(c, c))
	header := make([]byte, 80)
	copy(header[36:68], root[:])
	proof := append([]byte(nil), header...)
	proof = binary.LittleEndian.AppendUint32(proof, 3)
	proof = append(proof, 2)
	proof = append(proof, left[:]...)
	proof = append(proof, c[:]...)
	proof = append(proof, 1, 13)
	inc, err := decodeProof(proof, c.String())
	require.NoError(t, err)
	require.Equal(t, uint32(2), inc.Index)
	require.Equal(t, []string{c.String(), left.String()}, inc.MerkleBranch)
	got, err := VerifyInclusion(c.String(), inc)
	require.NoError(t, err)
	require.Equal(t, chainhash.DoubleHashH(header).String(), got)
	inc.MerkleBranch[0] = a.String()
	_, err = VerifyInclusion(c.String(), inc)
	require.Error(t, err)
}
