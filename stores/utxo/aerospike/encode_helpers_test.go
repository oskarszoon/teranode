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

func makeTxForSize(t *testing.T, nIn, nOut int, withPrevScript bool) *bt.Tx {
	t.Helper()
	tx := bt.NewTx()
	script, err := bscript.NewFromHexString("76a914000000000000000000000000000000000000000088ac")
	require.NoError(t, err)
	for i := 0; i < nIn; i++ {
		in := &bt.Input{PreviousTxOutIndex: uint32(i), PreviousTxSatoshis: 1000, SequenceNumber: 0xffffffff}
		in.UnlockingScript = script
		if withPrevScript {
			in.PreviousTxScript = script
		}
		// PreviousTxIDAddStr sets the 32-byte previousTxIDHash, which is required for
		// Input.Size() to match the actual serialized length (Bytes() skips it when nil).
		require.NoError(t, in.PreviousTxIDAddStr("0000000000000000000000000000000000000000000000000000000000000000"))
		tx.Inputs = append(tx.Inputs, in)
	}
	for i := 0; i < nOut; i++ {
		tx.Outputs = append(tx.Outputs, &bt.Output{Satoshis: 1, LockingScript: script})
	}
	return tx
}

func TestExtendedTxSizeMatchesExtendedBytes(t *testing.T) {
	cases := []struct {
		nIn, nOut  int
		prevScript bool
	}{
		{1, 1, true}, {1, 1, false}, {3, 5, true}, {2, 0, true}, {0, 2, true}, {5, 1, false},
	}
	for _, c := range cases {
		tx := makeTxForSize(t, c.nIn, c.nOut, c.prevScript)
		require.Equal(t, len(tx.ExtendedBytes()), extendedTxSize(tx),
			"nIn=%d nOut=%d prevScript=%v", c.nIn, c.nOut, c.prevScript)
	}
}

func TestExtendedTxSize_LargePrevScript(t *testing.T) {
	tx := bt.NewTx()
	big := make([]byte, 300) // > 252 => 3-byte VarInt length prefix
	for i := range big {
		big[i] = 0x51
	}
	bigScript := bscript.Script(big)
	in := &bt.Input{PreviousTxOutIndex: 0, PreviousTxSatoshis: 1000, SequenceNumber: 0xffffffff}
	in.UnlockingScript = &bscript.Script{}
	in.PreviousTxScript = &bigScript
	require.NoError(t, in.PreviousTxIDAddStr("0000000000000000000000000000000000000000000000000000000000000000"))
	tx.Inputs = append(tx.Inputs, in)
	script, err := bscript.NewFromHexString("76a914000000000000000000000000000000000000000088ac")
	require.NoError(t, err)
	tx.Outputs = append(tx.Outputs, &bt.Output{Satoshis: 1, LockingScript: script})

	require.Equal(t, len(tx.ExtendedBytes()), extendedTxSize(tx))
}

func TestExtendedTxSize_DecodedTxShape(t *testing.T) {
	// Round-trip a constructed extended tx through bytes so inputs carry a real
	// (non-nil) previousTxIDHash, matching the production decode path.
	src := makeTxForSize(t, 3, 4, true)
	decoded, err := bt.NewTxFromBytes(src.ExtendedBytes())
	require.NoError(t, err)
	require.Equal(t, len(decoded.ExtendedBytes()), extendedTxSize(decoded))

	// Coinbase-shaped tx: single input with 32 zero-byte prev txid (non-nil).
	cb := bt.NewTx()
	cbIn := &bt.Input{PreviousTxOutIndex: 0xffffffff, SequenceNumber: 0xffffffff}
	cbIn.UnlockingScript = &bscript.Script{0x03, 0x01, 0x02, 0x03}
	require.NoError(t, cbIn.PreviousTxIDAddStr("0000000000000000000000000000000000000000000000000000000000000000"))
	cb.Inputs = append(cb.Inputs, cbIn)
	script, err := bscript.NewFromHexString("76a914000000000000000000000000000000000000000088ac")
	require.NoError(t, err)
	cb.Outputs = append(cb.Outputs, &bt.Output{Satoshis: 5000000000, LockingScript: script})
	require.Equal(t, len(cb.ExtendedBytes()), extendedTxSize(cb))
}
