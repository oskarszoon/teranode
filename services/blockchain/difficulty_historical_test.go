package blockchain

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/teranode/model"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// Real headers include the first modern-DAA calculation at parent height 148,
// and continue past the first pinned testnet checkpoint at height 546.
func TestDifficultyHistoricalTestnet(t *testing.T) {
	ctx := context.Background()
	s := test.CreateBaseTestSettings(t)
	params := chaincfg.TestNetParams
	s.ChainCfgParams = &params
	store, err := blockchainstore.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, s)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.(interface{ Close() error }).Close()) })
	difficulty, err := NewDifficulty(store, ulogger.TestLogger{}, s)
	require.NoError(t, err)
	data, err := os.ReadFile("testdata/testnet_headers_0_547.bin")
	require.NoError(t, err)
	require.Len(t, data, 548*80)
	genesis, err := model.NewBlockFromMsgBlock(params.GenesisBlock, nil)
	require.NoError(t, err)
	// The SQL store identifies genesis by its coinbase txid.
	genesis.CoinbaseTx.LockTime = 1
	parent := genesis.Header
	for height := uint32(1); height <= 547; height++ {
		header, err := model.NewBlockHeaderFromBytes(data[height*80 : (height+1)*80])
		require.NoError(t, err)
		require.Equal(t, parent.Hash(), header.HashPrevBlock)
		require.NoError(t, header.HasMetPowLimit(&params))
		met, _, err := header.HasMetTargetDifficulty()
		require.NoError(t, err)
		require.True(t, met)
		if height == 149 {
			require.Equal(t, "00000000291d8e6f5d0d2a59de8f0f206917f3e00ff53edc8f6dcaddd61f3fe9", header.Hash().String())
		}
		if height == 546 {
			require.Equal(t, params.Checkpoints[0].Hash, header.Hash())
		}
		bits, err := difficulty.CalcNextWorkRequired(ctx, parent, height-1, int64(header.Timestamp))
		require.NoError(t, err, "height %d", height)
		require.Equal(t, header.Bits, *bits, "height %d", height)
		// Difficulty consumes headers only; the store requires a coinbase field.
		_, _, err = store.StoreBlock(ctx, &model.Block{Header: header, CoinbaseTx: genesis.CoinbaseTx, TransactionCount: 1, Height: height}, "test")
		require.NoError(t, err)
		parent = header
	}
}

// Synthetic timestamps isolate retarget boundaries without mining a chain.
// All ancestor lookups still use the real SQLite store.
func historicalDifficultyChain(t *testing.T, params chaincfg.Params, count uint32, spacing uint32) (*Difficulty, []*model.BlockHeader) {
	t.Helper()
	s := test.CreateBaseTestSettings(t)
	s.ChainCfgParams = &params
	store, err := blockchainstore.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, s)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.(interface{ Close() error }).Close()) })
	d, err := NewDifficulty(store, ulogger.TestLogger{}, s)
	require.NoError(t, err)
	genesis, err := model.NewBlockFromMsgBlock(params.GenesisBlock, nil)
	require.NoError(t, err)
	genesis.CoinbaseTx.LockTime = 1
	headers := []*model.BlockHeader{genesis.Header}
	bits, err := model.NewNBitFromString("1c0ffff0")
	require.NoError(t, err)
	for height := uint32(1); height <= count; height++ {
		header := *genesis.Header
		header.HashPrevBlock = headers[height-1].Hash()
		header.Timestamp += height * spacing
		if spacing == 0 {
			header.Timestamp -= height
		}
		header.Bits = *bits
		header.Nonce = height
		_, _, err := store.StoreBlock(context.Background(), &model.Block{Header: &header, CoinbaseTx: genesis.CoinbaseTx, TransactionCount: 1, Height: height}, "test")
		require.NoError(t, err)
		headers = append(headers, &header)
	}
	return d, headers
}

func TestDifficultyHistoricalRetarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spacing uint32
		want    string
	}{
		{"ordinary retarget", 600, "1c0ffde7"},
		{"fast clamp", 1, "1c03fffc"},
		{"slow clamp", 3000, "1c3fffc0"},
		{"negative timespan", 0, "1c03fffc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := chaincfg.TestNetParams
			d, headers := historicalDifficultyChain(t, params, 2015, tc.spacing)
			parent := *headers[2015]
			// A delayed testnet timestamp must not bypass the periodic retarget.
			bits, err := d.CalcNextWorkRequired(t.Context(), &parent, 2015, int64(parent.Timestamp)+1201)
			require.NoError(t, err)
			require.Equal(t, tc.want, bits.String())
		})
	}
}

func TestDifficultyHistoricalTestnetRestoresTarget(t *testing.T) {
	params := chaincfg.TestNetParams
	d, headers := historicalDifficultyChain(t, params, 20, 600)
	parent := headers[20]
	// A special minimum-difficulty block follows an ordinary target.
	parent.Bits = *d.powLimitnBits
	for _, tc := range []struct {
		delay int64
		want  string
	}{{1200, "1c0ffff0"}, {1201, "1d00ffff"}} {
		bits, err := d.CalcNextWorkRequired(t.Context(), parent, 20, int64(parent.Timestamp)+tc.delay)
		require.NoError(t, err)
		require.Equal(t, tc.want, bits.String())
	}
}

func TestDifficultyHistoricalEDAActivation(t *testing.T) {
	params := chaincfg.MainNetParams
	params.UahfForkHeight = 20
	params.DaaForkHeight = 40
	for _, spacing := range []uint32{7199, 7200} {
		d, headers := historicalDifficultyChain(t, params, 20, spacing)
		for _, height := range []uint32{19, 20} {
			bits, err := d.CalcNextWorkRequired(t.Context(), headers[height], height, int64(headers[height].Timestamp)+600)
			require.NoError(t, err)
			want := "1c0ffff0"
			if height == 20 && spacing == 7200 {
				want = "1c13ffec"
			}
			require.Equal(t, want, bits.String(), "parent %d, spacing %d", height, spacing)
		}
	}
}

func TestDifficultyHistoricalDAAActivation(t *testing.T) {
	params := chaincfg.MainNetParams
	params.UahfForkHeight = 0
	params.DaaForkHeight = 150
	d, headers := historicalDifficultyChain(t, params, 150, 300)
	for _, tc := range []struct {
		height uint32
		want   string
	}{{149, "1c0ffff0"}, {150, "1c07fff8"}} {
		bits, err := d.CalcNextWorkRequired(t.Context(), headers[tc.height], tc.height, int64(headers[tc.height].Timestamp)+300)
		require.NoError(t, err)
		require.Equal(t, tc.want, bits.String(), "parent %d", tc.height)
	}
}

func TestDifficultyHistoricalMissingHistory(t *testing.T) {
	params := chaincfg.TestNetParams
	d, headers := historicalDifficultyChain(t, params, 20, 600)
	parent := *headers[20]
	parent.Bits = *d.powLimitnBits
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bits, err := d.CalcNextWorkRequired(ctx, &parent, 20, int64(parent.Timestamp)+600)
	require.Error(t, err)
	require.Nil(t, bits, "unavailable ancestry cannot silently grant minimum difficulty")
}

func TestDifficultyHistoricalRetargetPowLimit(t *testing.T) {
	params := chaincfg.TestNetParams
	params.PowLimitBits = 0x1c0ffff0
	params.PowLimit = CompactToBig(params.PowLimitBits)
	d, headers := historicalDifficultyChain(t, params, 2015, 3000)
	bits, err := d.CalcNextWorkRequired(t.Context(), headers[2015], 2015, int64(headers[2015].Timestamp)+600)
	require.NoError(t, err)
	require.Equal(t, "1c0ffff0", bits.String(), "a historical retarget must respect the network PoW limit")
}
