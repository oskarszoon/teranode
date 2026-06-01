package aerospike

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/stretchr/testify/require"
)

func TestOutputSizeEqualsBytesLen(t *testing.T) {
	scripts := []string{
		"",
		"76a914000000000000000000000000000000000000000088ac",
		"6a4c64" + "00",
	}
	for _, hexScript := range scripts {
		s, err := bscript.NewFromHexString(hexScript)
		require.NoError(t, err)
		out := &bt.Output{Satoshis: 12345, LockingScript: s}
		require.Equal(t, len(out.Bytes()), out.Size())
	}
}
