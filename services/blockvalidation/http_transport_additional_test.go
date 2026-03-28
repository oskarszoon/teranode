package blockvalidation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	bt "github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/stretchr/testify/require"
)

// buildProperBlockBytes constructs a block using the model's own serialization
// so it can be round-tripped via model.NewBlockFromReader reliably.
func buildProperBlockBytes(t *testing.T) []byte {
	t.Helper()

	coinbaseTx := bt.NewTx()
	require.NoError(t, coinbaseTx.From(
		"0000000000000000000000000000000000000000000000000000000000000000",
		0xffffffff,
		"00",
		0,
	))
	coinbaseTx.AddOutput(&bt.Output{
		Satoshis:      0,
		LockingScript: bscript.NewFromBytes([]byte{0x6a}), // OP_RETURN
	})

	block, err := model.NewBlock(
		model.GenesisBlockHeader,
		coinbaseTx,
		nil, // no subtrees
		1,   // txCount
		0,   // sizeInBytes
		0,   // height
		0,   // id
	)
	require.NoError(t, err)

	blockBytes, err := block.Bytes()
	require.NoError(t, err)
	return blockBytes
}

// ---------------------------------------------------------------------------
// FetchBlock
// ---------------------------------------------------------------------------

func TestHTTPTransport_FetchBlock_Success(t *testing.T) {
	blockBytes := buildProperBlockBytes(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/block/")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(blockBytes)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	block, err := tr.FetchBlock(context.Background(), srv.URL, hash)
	require.NoError(t, err)
	require.NotNil(t, block)
}

func TestHTTPTransport_FetchBlock_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchBlock(context.Background(), srv.URL, hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to fetch block")
}

func TestHTTPTransport_FetchBlock_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not a valid block"))
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchBlock(context.Background(), srv.URL, hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse block")
}

// ---------------------------------------------------------------------------
// FetchBlocks
// ---------------------------------------------------------------------------

func TestHTTPTransport_FetchBlocks_Success(t *testing.T) {
	single := buildProperBlockBytes(t)
	twoBlocks := make([]byte, 0, len(single)*2)
	twoBlocks = append(twoBlocks, single...)
	twoBlocks = append(twoBlocks, single...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/blocks/")
		require.Contains(t, r.URL.RawQuery, "n=2")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(twoBlocks)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	blocks, err := tr.FetchBlocks(context.Background(), srv.URL, hash, 2)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
}

func TestHTTPTransport_FetchBlocks_ThreeBlocks(t *testing.T) {
	single := buildProperBlockBytes(t)
	threeBlocks := make([]byte, 0, len(single)*3)
	threeBlocks = append(threeBlocks, single...)
	threeBlocks = append(threeBlocks, single...)
	threeBlocks = append(threeBlocks, single...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(threeBlocks)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	blocks, err := tr.FetchBlocks(context.Background(), srv.URL, hash, 3)
	require.NoError(t, err)
	require.Len(t, blocks, 3)
}

func TestHTTPTransport_FetchBlocks_SingleBlock(t *testing.T) {
	single := buildProperBlockBytes(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(single)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	blocks, err := tr.FetchBlocks(context.Background(), srv.URL, hash, 1)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
}

func TestHTTPTransport_FetchBlocks_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchBlocks(context.Background(), srv.URL, hash, 5)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to fetch blocks")
}

func TestHTTPTransport_FetchBlocks_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Empty body — zero blocks.
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	blocks, err := tr.FetchBlocks(context.Background(), srv.URL, hash, 5)
	require.NoError(t, err)
	require.Empty(t, blocks)
}

// ---------------------------------------------------------------------------
// FetchSubtree — additional error cases
// ---------------------------------------------------------------------------

func TestHTTPTransport_FetchSubtree_HTTPError_BadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchSubtree(context.Background(), srv.URL, hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to fetch subtree")
}

// ---------------------------------------------------------------------------
// FetchSubtreeData — additional error cases
// ---------------------------------------------------------------------------

func TestHTTPTransport_FetchSubtreeData_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchSubtreeData(context.Background(), srv.URL, hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to fetch subtree data")
}

func TestHTTPTransport_FetchSubtreeData_ReadContent(t *testing.T) {
	payload := []byte("large-subtree-payload-for-streaming")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/subtree_data/")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	rc, err := tr.FetchSubtreeData(context.Background(), srv.URL, hash)
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

// ---------------------------------------------------------------------------
// URL construction
// ---------------------------------------------------------------------------

func TestHTTPTransport_URLConstruction_FetchBlock(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buildProperBlockBytes(t))
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, _ = tr.FetchBlock(context.Background(), srv.URL, hash)
	require.Equal(t, "/block/"+hash.String(), capturedPath)
}

func TestHTTPTransport_URLConstruction_FetchBlocks(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, _ = tr.FetchBlocks(context.Background(), srv.URL, hash, 7)
	require.Contains(t, capturedURL, "/blocks/"+hash.String())
	require.Contains(t, capturedURL, "n=7")
}

func TestHTTPTransport_URLConstruction_FetchSubtreeData(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	rc, err := tr.FetchSubtreeData(context.Background(), srv.URL, hash)
	require.NoError(t, err)
	defer rc.Close()

	require.Equal(t, "/subtree_data/"+hash.String(), capturedPath)
}

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestHTTPTransport_FetchBlock_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buildProperBlockBytes(t))
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := tr.FetchBlock(ctx, srv.URL, hash)
	require.Error(t, err)
}
