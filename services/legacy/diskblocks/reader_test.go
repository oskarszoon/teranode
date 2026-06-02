package diskblocks

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

func writeBlkFile(t *testing.T, datadir string, fileNo int, net wire.BitcoinNet, blk *wire.MsgBlock) uint32 {
	t.Helper()
	var payload bytes.Buffer
	require.NoError(t, blk.BsvEncode(&payload, wire.ProtocolVersion, wire.BaseEncoding))

	var framed bytes.Buffer
	require.NoError(t, binary.Write(&framed, binary.LittleEndian, uint32(net)))
	require.NoError(t, binary.Write(&framed, binary.LittleEndian, uint32(payload.Len())))
	dataPos := uint32(framed.Len()) // payload starts here
	framed.Write(payload.Bytes())

	blocksDir := filepath.Join(datadir, "blocks")
	require.NoError(t, os.MkdirAll(blocksDir, 0o755))
	path := filepath.Join(blocksDir, "blk00000.dat")
	require.NoError(t, os.WriteFile(path, framed.Bytes(), 0o644))
	return dataPos
}

func TestReadBlockRoundTrip(t *testing.T) {
	net := chaincfg.TestNetParams.Net
	blk := wire.NewMsgBlock(&wire.BlockHeader{Version: 1})
	datadir := t.TempDir()
	dataPos := writeBlkFile(t, datadir, 0, net, blk)

	br := NewBlockReader(datadir, net)
	got, n, err := br.ReadBlock(&BlockRef{NFile: 0, NDataPos: dataPos})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Greater(t, n, uint64(8))
	require.Equal(t, blk.Header.Version, got.Header.Version)
}

func TestReadBlockMagicMismatch(t *testing.T) {
	blk := wire.NewMsgBlock(&wire.BlockHeader{Version: 1})
	datadir := t.TempDir()
	dataPos := writeBlkFile(t, datadir, 0, chaincfg.MainNetParams.Net, blk)

	br := NewBlockReader(datadir, chaincfg.TestNetParams.Net) // wrong net
	_, _, err := br.ReadBlock(&BlockRef{NFile: 0, NDataPos: dataPos})
	require.Error(t, err)
}
