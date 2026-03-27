package blockvalidation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/stretchr/testify/require"
)

// genesisHeaderBytes returns the 80-byte serialization of the Bitcoin genesis block header.
// It is known-valid PoW and can be used anywhere a real header is needed in tests.
func genesisHeaderBytes(t *testing.T) []byte {
	t.Helper()
	return model.GenesisBlockHeader.Bytes()
}

// makeHash returns a deterministic chainhash for tests.
func makeHash(t *testing.T) *chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000001")
	require.NoError(t, err)
	return h
}

func TestHTTPTransport_FetchHeaders_Success(t *testing.T) {
	hdrBytes := genesisHeaderBytes(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/headers_from_common_ancestor/")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(hdrBytes)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	headers, err := tr.FetchHeaders(context.Background(), srv.URL, hash, []*chainhash.Hash{hash}, 10000)
	require.NoError(t, err)
	require.Len(t, headers, 1)
}

func TestHTTPTransport_FetchHeaders_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// No body — peer has no headers to send.
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	headers, err := tr.FetchHeaders(context.Background(), srv.URL, hash, []*chainhash.Hash{hash}, 10000)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestHTTPTransport_FetchHeaders_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchHeaders(context.Background(), srv.URL, hash, []*chainhash.Hash{hash}, 10000)
	require.Error(t, err)
}

func TestHTTPTransport_FetchSubtree_Success(t *testing.T) {
	expected := []byte("subtree-data-bytes")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/subtree/")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(expected)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	got, err := tr.FetchSubtree(context.Background(), srv.URL, hash)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

func TestHTTPTransport_FetchSubtreeData_Success(t *testing.T) {
	expected := []byte("subtree-data-stream-bytes")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/subtree_data/")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(expected)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	rc, err := tr.FetchSubtreeData(context.Background(), srv.URL, hash)
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

func TestHTTPTransport_FetchSubtree_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	hash := makeHash(t)

	_, err := tr.FetchSubtree(context.Background(), srv.URL, hash)
	require.Error(t, err)
}

func TestHTTPTransport_URLConstruction_FetchHeaders(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := NewHTTPTransport()
	target, _ := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000002")
	locator, _ := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000001")

	_, _ = tr.FetchHeaders(context.Background(), srv.URL, target, []*chainhash.Hash{locator}, 500)

	require.Contains(t, capturedURL, "/headers_from_common_ancestor/"+target.String())
	require.Contains(t, capturedURL, "n=500")
	require.Contains(t, capturedURL, locator.String())
}
