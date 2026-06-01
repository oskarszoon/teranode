package aerospike

import (
	"os"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/teranode/util/test"
)

// TestCreateArena_ConcurrentReuseNoCorruption verifies that the pool's
// get/put cycle is race-free and that arena-backed slices produced within
// a single lease hold the correct bytes before the arena is returned.
func TestCreateArena_ConcurrentReuseNoCorruption(t *testing.T) {
	script, err := bscript.NewFromHexString("76a914000000000000000000000000000000000000000088ac")
	require.NoError(t, err)

	var wg sync.WaitGroup

	for g := 0; g < 8; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < 50; i++ {
				a := getCreateArena()
				out := &bt.Output{Satoshis: uint64(g*100 + i), LockingScript: script}
				got := appendOutputInto(a, out)
				require.Equal(t, out.Bytes(), got) // bytes correct despite reuse
				putCreateArena(a)
			}
		}(g)
	}

	wg.Wait()
}

// TestGetBinsToStore_ArenaMatchesNoArena asserts that GetBinsToStore produces
// byte-identical bin values whether or not an arena is supplied.
// The Store is constructed the same way TestStore_GetBinsToStore does it —
// no Docker required.
func TestGetBinsToStore_ArenaMatchesNoArena(t *testing.T) {
	InitPrometheusMetrics()

	txHex, err := os.ReadFile("testdata/fbebcc148e40cb6c05e57c6ad63abd49d5e18b013c82f704601bc4ba567dfb90.hex")
	require.NoError(t, err)

	tx, err := bt.NewTxFromString(string(txHex))
	require.NoError(t, err)

	s := Store{}
	s.SetUtxoBatchSize(100)
	s.SetSettings(test.CreateBaseTestSettings(t))

	want, err := s.GetBinsToStore(tx, 0, nil, nil, nil, false, tx.TxIDChainHash(), false, false, false, nil)
	require.NoError(t, err)

	arena := bt.NewArena(0)

	got, err := s.GetBinsToStore(tx, 0, nil, nil, nil, false, tx.TxIDChainHash(), false, false, false, arena)
	require.NoError(t, err)

	// Structural equality: same number of record groups and same number of bins per group.
	require.Equal(t, len(want), len(got), "number of bin groups differs")

	for ri := range want {
		require.Equal(t, len(want[ri]), len(got[ri]), "number of bins in group %d differs", ri)

		for bi, wb := range want[ri] {
			gb := got[ri][bi]
			require.Equal(t, wb.Name, gb.Name, "bin name mismatch at [%d][%d]", ri, bi)

			// The only scalar []byte bin is txID (txHash[:]); compare it by
			// content so pointer identity differences don't matter.
			// The arena-backed bytes live in inputs/outputs, which are stored
			// as aerospike ListValue ([]interface{} of []byte chunks). Those
			// bins fall through to the else branch where require.Equal recurses
			// via reflect.DeepEqual into the nested byte slices — a single
			// flipped byte in any chunk causes a failure there.
			wBytes, wOk := wb.Value.GetObject().([]byte)
			gBytes, gOk := gb.Value.GetObject().([]byte)

			if wOk && gOk {
				require.Equal(t, wBytes, gBytes, "byte content mismatch for bin %q at [%d][%d]", wb.Name, ri, bi)
			} else {
				// Integers, booleans, and list values (inputs/outputs) — deep equality
				// covers the nested []byte slices inside the list entries.
				require.Equal(t, wb.Value, gb.Value, "value mismatch for bin %q at [%d][%d]", wb.Name, ri, bi)
			}
		}
	}
}
