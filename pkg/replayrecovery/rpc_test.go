package replayrecovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func rpcFixture(t *testing.T, mode string) (*RPCSource, string, Tip) {
	t.Helper()
	raw := "0100000001" + strings.Repeat("00", 32) + "ffffffff0100ffffffff010100000000000000015100000000"
	tx, err := bt.NewTxFromString(raw)
	require.NoError(t, err)
	header := make([]byte, 80)
	copy(header[36:68], tx.TxIDChainHash()[:])
	block := chainhash.DoubleHashH(header).String()
	tip := Tip{Hash: block, Height: 10}
	proof := append([]byte(nil), header...)
	proof = binary.LittleEndian.AppendUint32(proof, 1)
	proof = append(proof, 1)
	proof = append(proof, tx.TxIDChainHash()[:]...)
	proof = append(proof, 1, 1)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&q))
		var result any
		var rpcErr any
		switch q.Method {
		case "getblockchaininfo":
			calls++
			h := block
			if mode == "tip-change" && calls > 1 {
				h = strings.Repeat("ab", 32)
			}
			result = map[string]any{"bestblockhash": h, "blocks": 10, "chain": "regtest", "initialblockdownload": false}
		case "getrawtransaction":
			result = map[string]any{"hex": raw, "blockhash": block, "confirmations": 1}
			if mode == "pruned" {
				result = nil
				rpcErr = map[string]any{"code": -5, "message": "pruned"}
			}
			if mode == "unconfirmed" {
				result = map[string]any{"hex": raw, "confirmations": 0}
			}
			if mode == "wrong-tx" {
				result = map[string]any{"hex": strings.Replace(raw, "0151", "0152", 1), "blockhash": block, "confirmations": 1}
			}
		case "getblockheader":
			result = map[string]any{"hash": block, "height": 10, "confirmations": 1}
		case "getblockhash":
			result = block
			if mode == "fork" {
				result = strings.Repeat("aa", 32)
			}
		case "gettxoutproof":
			p := append([]byte(nil), proof...)
			if mode == "bad-proof" {
				p[36] ^= 1
			}
			result = hex.EncodeToString(p)
		case "gettxout":
			require.JSONEq(t, "false", string(q.Params[2]))
			if mode == "live" || strings.HasPrefix(mode, "bad-output") {
				result = map[string]any{"bestblock": block, "confirmations": 1, "value": 0.00000001, "scriptPubKey": map[string]any{"hex": "51"}}
				if mode == "bad-output-value" {
					result.(map[string]any)["value"] = 1
				}
				if mode == "bad-output-tip" {
					result.(map[string]any)["bestblock"] = strings.Repeat("ab", 32)
				}
				if mode == "bad-output-script" {
					result.(map[string]any)["scriptPubKey"] = map[string]any{"hex": "52"}
				}
			}
		default:
			t.Errorf("unexpected RPC %s", q.Method)
		}
		if mode == "duplicate-envelope" {
			_, _ = fmt.Fprintf(w, `{"id":%d,"result":null,"result":%s,"error":null}`, q.ID, mustJSON(result))
			return
		}
		if mode == "wrong-id" {
			q.ID++
		}
		if mode == "malformed" {
			_, _ = w.Write([]byte("{"))
			return
		}
		if mode == "oversized" {
			_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": q.ID, "result": result, "error": rpcErr}))
	}))
	t.Cleanup(server.Close)
	source, err := NewRPCSource(server.URL, server.Client(), nil)
	require.NoError(t, err)
	require.NoError(t, source.Configure(RPCOptions{RequestsPerSecond: 100000, MaxResponseBytes: 1024, Concurrency: 2, Timeout: time.Second}))
	return source, tx.TxID(), tip
}

func TestRPCClassification(t *testing.T) {
	for _, mode := range []string{"spent", "live", "pruned", "wrong-tx", "bad-proof", "tip-change", "malformed", "oversized", "fork", "bad-output-value", "bad-output-tip", "bad-output-script", "wrong-id", "duplicate-envelope", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			source, id, tip := rpcFixture(t, mode)
			ev, err := source.Check(context.Background(), id, tip)
			switch mode {
			case "spent":
				require.NoError(t, err)
				require.Equal(t, FullySpent, ev.Classification)
			case "live":
				require.NoError(t, err)
				require.Equal(t, Live, ev.Classification)
			default:
				require.Equal(t, Unknown, ev.Classification)
				require.NotEmpty(t, ev.Reason)
			}
		})
	}
}
func TestRPCCancellation(t *testing.T) {
	source, id, tip := rpcFixture(t, "spent")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev, err := source.Check(ctx, id, tip)
	require.Error(t, err)
	require.Equal(t, Unknown, ev.Classification)
}

func TestRPCUnspent(t *testing.T) {
	for _, mode := range []string{"live", "spent", "tip-change"} {
		t.Run(mode, func(t *testing.T) {
			source, id, tip := rpcFixture(t, mode)
			live, err := source.Unspent(context.Background(), id, 0, tip)
			if mode == "tip-change" {
				require.Error(t, err)
				require.False(t, live)
			} else {
				require.NoError(t, err)
				require.Equal(t, mode == "live", live)
			}
		})
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func TestRPCUnconfirmedRetainsRaw(t *testing.T) {
	s, id, tip := rpcFixture(t, "unconfirmed")
	e, err := s.Check(context.Background(), id, tip)
	require.Error(t, err)
	require.NotEmpty(t, e.RawTx)
	require.Equal(t, Unknown, e.Classification)
}

func TestRPCConcurrencyBound(t *testing.T) {
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, count) {
				break
			}
		}
		var request struct {
			ID uint64 `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		time.Sleep(15 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": request.ID, "result": map[string]any{"bestblockhash": strings.Repeat("00", 32), "blocks": 1, "chain": "regtest"}, "error": nil})
	}))
	defer server.Close()
	source, err := NewRPCSource(server.URL, server.Client(), nil)
	require.NoError(t, err)
	require.NoError(t, source.Configure(RPCOptions{Concurrency: 2, RequestsPerSecond: 100000, MaxResponseBytes: 1024, Timeout: time.Second}))
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := source.Tip(context.Background()); failures <- e }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	require.Equal(t, int32(2), maximum.Load())
}

func TestRPCDiscoverRetainsUnresolvedAudit(t *testing.T) {
	source, id, tip := rpcFixture(t, "unconfirmed")
	backend, _, assembly, _, manifest, _ := recoveryFixture(t)
	backend.seeds = []string{id, strings.Repeat("ff", 32)}
	assembly.ids = backend.seeds
	assembly.state.Tip = tip
	summary, err := Discover(context.Background(), backend, source, assembly, manifest, nil)
	require.ErrorIs(t, err, ErrIncomplete)
	require.Equal(t, int64(2), summary.Scanned)
	require.Equal(t, int64(2), summary.Unknown)
	require.Zero(t, backend.writes)
	var output bytes.Buffer
	require.NoError(t, ExportManifest(context.Background(), manifest, &output))
	require.Contains(t, output.String(), "historical confirmation unavailable")
}
