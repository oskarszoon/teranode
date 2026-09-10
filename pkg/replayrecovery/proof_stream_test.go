package replayrecovery

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBoundedStreamConsumesExactlyOne(t *testing.T) {
	wire, err := hex.DecodeString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0100ffffffff010100000000000000015100000000")
	require.NoError(t, err)
	r := bytes.NewReader(append(append([]byte{}, wire...), wire...))
	tx, err := ReadBoundedTransactionStream(r, 1024)
	require.NoError(t, err)
	require.Equal(t, wire, tx.Bytes())
	require.Equal(t, len(wire), r.Len())
	_, err = ReadBoundedTransactionStream(r, 20)
	require.Error(t, err)
}
func TestBoundedStreamRejectsHugeCounts(t *testing.T) {
	for _, wire := range [][]byte{{1, 0, 0, 0, 255, 255, 255, 255, 255, 255, 255, 255, 127}, {1, 0, 0, 0, 1, 0}} {
		_, err := ReadBoundedTransactionStream(bytes.NewReader(wire), 1<<20)
		require.Error(t, err)
	}
}
