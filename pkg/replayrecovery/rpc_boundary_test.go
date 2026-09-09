package replayrecovery

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRPCBoundaryRejectsMalformedEnvelopes(t *testing.T) {
	tests := []struct{ name, body, reason string }{
		{"array envelope", `[]`, "malformed RPC envelope"},
		{"non-object error", `{"id":1,"error":true,"result":null}`, "malformed RPC error"},
		{"missing result", `{"id":1,"error":null}`, "missing RPC result"},
		{"wrong result type", `{"id":1,"error":null,"result":"tip"}`, "malformed RPC getblockchaininfo result"},
		{"empty body", "", "malformed RPC JSON"},
		{"truncated nested array", `{"id":1,"result":[`, "malformed RPC array"},
		{"nested duplicate key", `{"id":1,"result":{"blocks":1,"blocks":2}}`, "duplicate RPC JSON key"},
		{"excessive nesting", `{"id":1,"result":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}", "RPC JSON nesting exceeds limit"},
		{"trailing response", `{"id":1,"result":null} {}`, "trailing RPC JSON"},
		{"syncing node", `{"id":1,"result":{"bestblockhash":"` + strings.Repeat("ab", 32) + `","blocks":10,"chain":"regtest","initialblockdownload":true}}`, "RPC chain state unavailable or syncing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, test.body) }))
			defer server.Close()
			source, err := NewRPCSource(server.URL, server.Client(), nil)
			require.NoError(t, err)
			tip, err := source.Tip(context.Background())
			require.ErrorContains(t, err, test.reason)
			require.Empty(t, tip.Hash)
		})
	}
}

func TestRPCBoundaryRejectsHTTPFailuresAndRedirects(t *testing.T) {
	for _, mode := range []string{"unavailable", "truncated body", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var targetCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "unavailable":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "truncated body":
					w.Header().Set("Content-Length", "1000")
					_, _ = fmt.Fprint(w, `{"id":1}`)
				case "redirect":
					http.Redirect(w, r, target.URL+"/?credential=do-not-log", http.StatusFound)
				}
			}))
			defer server.Close()
			source, err := NewRPCSource(server.URL, nil, nil)
			require.NoError(t, err)
			tip, err := source.Tip(context.Background())
			require.Error(t, err)
			require.Empty(t, tip.Hash)
			require.NotContains(t, err.Error(), "do-not-log")
			require.Zero(t, targetCalls.Load())
			switch mode {
			case "unavailable":
				require.ErrorContains(t, err, "HTTP status 503")
			case "truncated body":
				require.ErrorContains(t, err, "response read failed")
			case "redirect":
				require.ErrorContains(t, err, "transport failure")
			}
		})
	}
}

func TestRPCBoundaryRejectsUnsafeConfiguration(t *testing.T) {
	for _, endpoint := range []string{"", "file:///tmp/rpc", "https://example.invalid/#fragment"} {
		source, err := NewRPCSource(endpoint, nil, nil)
		require.Error(t, err)
		require.Nil(t, source)
	}
	source, err := NewRPCSource("http://127.0.0.1:1", nil, nil)
	require.NoError(t, err)
	for _, rate := range []float64{0, math.NaN(), math.Inf(1), 100001} {
		require.Error(t, source.Configure(RPCOptions{MaxResponseBytes: 1024, RequestsPerSecond: rate, Concurrency: 1, Timeout: time.Second}))
	}
	ev, err := source.Check(context.Background(), "bad id", Tip{})
	require.Error(t, err)
	require.Equal(t, Unknown, ev.Classification)
}
