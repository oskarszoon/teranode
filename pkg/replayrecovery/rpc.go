package replayrecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
)

// History never infers non-confirmation from absent archives. Lookup must return
// an error when the requested canonical inclusion is unavailable.
type History interface {
	Lookup(context.Context, string, Tip) (Inclusion, error)
}

type RPCOptions struct {
	MaxResponseBytes  int64
	RequestsPerSecond float64
	Concurrency       int
	Timeout           time.Duration
}

// RPCSource treats the configured endpoint as a trusted current UTXO view.
// Configuration must finish before the source is used concurrently.
type RPCSource struct {
	endpoint string
	client   *http.Client
	history  History
	options  RPCOptions
	slots    chan struct{}
	mu       sync.Mutex
	next     time.Time
	id       atomic.Uint64
}

func NewRPCSource(endpoint string, client *http.Client, history History) (*RPCSource, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, fmt.Errorf("invalid RPC endpoint")
	}
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return fmt.Errorf("RPC redirects disabled") }
	s := &RPCSource{endpoint: endpoint, client: &copied, history: history}
	_ = s.Configure(RPCOptions{MaxResponseBytes: 16 << 20, RequestsPerSecond: 5, Concurrency: 4, Timeout: 30 * time.Second})
	return s, nil
}
func (s *RPCSource) Configure(o RPCOptions) error {
	if o.MaxResponseBytes < 256 || o.MaxResponseBytes > 256<<20 || math.IsNaN(o.RequestsPerSecond) || math.IsInf(o.RequestsPerSecond, 0) || o.RequestsPerSecond <= 0 || o.RequestsPerSecond > 100000 || o.Concurrency < 1 || o.Concurrency > 64 || o.Timeout <= 0 {
		return fmt.Errorf("invalid RPC limits")
	}
	s.options = o
	s.slots = make(chan struct{}, o.Concurrency)
	return nil
}
func (s *RPCSource) call(ctx context.Context, method string, params any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	now := time.Now()
	scheduled := s.next
	if scheduled.Before(now) {
		scheduled = now
	}
	s.next = scheduled.Add(time.Duration(float64(time.Second) / s.options.RequestsPerSecond))
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(scheduled))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	id := s.id.Add(1)
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("construct RPC request")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("RPC %s transport failure: %w", method, ctxError(ctx, err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("RPC %s HTTP status %d", method, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.options.MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("RPC response read failed")
	}
	if int64(len(raw)) > s.options.MaxResponseBytes {
		return fmt.Errorf("RPC response exceeds limit")
	}
	if err = strictRPCJSON(raw); err != nil {
		return err
	}
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("malformed RPC envelope")
	}
	var responseID uint64
	if json.Unmarshal(envelope.ID, &responseID) != nil || responseID != id {
		return fmt.Errorf("RPC response id mismatch")
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		var e struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(envelope.Error, &e) != nil {
			return fmt.Errorf("malformed RPC error")
		}
		return fmt.Errorf("RPC %s error %d", method, e.Code)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("missing RPC result")
	}
	if err = json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("malformed RPC %s result", method)
	}
	return nil
}
func ctxError(ctx context.Context, _ error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("request failed")
}
func (s *RPCSource) Tip(ctx context.Context) (Tip, error) {
	var info struct {
		Hash   string  `json:"bestblockhash"`
		Height *uint32 `json:"blocks"`
		Chain  string  `json:"chain"`
		IBD    bool    `json:"initialblockdownload"`
	}
	if err := s.call(ctx, "getblockchaininfo", []any{}, &info); err != nil {
		return Tip{}, err
	}
	if _, err := strictHash(info.Hash); err != nil || info.Height == nil || info.Chain == "" || info.IBD {
		return Tip{}, fmt.Errorf("RPC chain state unavailable or syncing")
	}
	return Tip{Hash: info.Hash, Height: *info.Height}, nil
}
func (s *RPCSource) stable(ctx context.Context, want Tip) error {
	got, err := s.Tip(ctx)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("RPC tip changed or disagrees")
	}
	return nil
}

func (s *RPCSource) Check(ctx context.Context, id string, tip Tip) (ev Evidence, err error) {
	ev = Evidence{TxID: id, Tip: tip, Classification: Unknown, Source: "rpc"}
	defer func() {
		if err != nil {
			ev.Classification = Unknown
			ev.Reason = err.Error()
		}
	}()
	if _, err = strictHash(id); err != nil {
		return ev, err
	}
	if err = s.stable(ctx, tip); err != nil {
		return ev, err
	}
	var raw struct {
		Hex           string `json:"hex"`
		BlockHash     string `json:"blockhash"`
		Confirmations int64  `json:"confirmations"`
	}
	historyErr := s.call(ctx, "getrawtransaction", []any{id, true}, &raw)
	var tx *bt.Tx
	if historyErr == nil && raw.Hex != "" {
		tx, err = checkedTransaction(id, raw.Hex)
		if err != nil {
			return ev, err
		}
		ev.RawTx = raw.Hex
	}
	var inclusion Inclusion
	if historyErr == nil && raw.BlockHash != "" && raw.Confirmations > 0 {
		var proof string
		historyErr = s.call(ctx, "gettxoutproof", []any{[]string{id}, raw.BlockHash}, &proof)
		if historyErr == nil {
			var b []byte
			b, err = hex.DecodeString(proof)
			if err != nil {
				return ev, fmt.Errorf("invalid proof hex")
			}
			inclusion, err = decodeProof(b, id)
			if err != nil {
				return ev, err
			}
			inclusion.RawTx = raw.Hex
			var block string
			block, err = VerifyInclusion(id, inclusion)
			if err != nil || block != raw.BlockHash {
				return ev, fmt.Errorf("proof block mismatch")
			}
			var h struct {
				Height        *uint32 `json:"height"`
				Confirmations int64   `json:"confirmations"`
			}
			historyErr = s.call(ctx, "getblockheader", []any{block, true}, &h)
			if historyErr == nil {
				if h.Height == nil || h.Confirmations <= 0 {
					return ev, fmt.Errorf("block not confirmed")
				}
				inclusion.Height = *h.Height
			}
		}
	} else {
		historyErr = fmt.Errorf("historical confirmation unavailable")
	}
	if historyErr != nil {
		if s.history == nil {
			return ev, historyErr
		}
		inclusion, err = s.history.Lookup(ctx, id, tip)
		if err != nil {
			return ev, fmt.Errorf("RPC history unavailable; local history: %w", err)
		}
		ev.Source = "local-history+rpc"
	}
	tx, err = checkedTransaction(id, inclusion.RawTx)
	if err != nil {
		return ev, err
	}
	ev.RawTx = inclusion.RawTx
	block, err := VerifyInclusion(id, inclusion)
	if err != nil {
		return ev, err
	}
	if inclusion.Height > tip.Height {
		return ev, fmt.Errorf("confirmation beyond tip")
	}
	var canonical string
	if err = s.call(ctx, "getblockhash", []any{inclusion.Height}, &canonical); err != nil {
		return ev, err
	}
	if canonical != block {
		return ev, fmt.Errorf("confirmation not canonical")
	}
	ev.BlockHash = block
	ev.BlockHeight = inclusion.Height
	live := false
	// Query every output, including provably unspendable scripts. Null on those
	// outputs is harmless; avoiding script heuristics also avoids era mismatches.
	for i, output := range tx.Outputs {
		var state *struct {
			BestBlock     string      `json:"bestblock"`
			Confirmations int64       `json:"confirmations"`
			Value         json.Number `json:"value"`
			Script        struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		}
		if err = s.call(ctx, "gettxout", []any{id, i, false}, &state); err != nil {
			return ev, err
		}
		if state == nil {
			continue
		}
		if state.BestBlock != tip.Hash || state.Confirmations <= 0 {
			return ev, fmt.Errorf("inconsistent output tip or confirmation")
		}
		value, amountErr := rpcAmount(state.Value)
		if amountErr != nil {
			return ev, amountErr
		}
		if value != output.Satoshis {
			return ev, fmt.Errorf("output amount mismatch")
		}
		script, e := hex.DecodeString(state.Script.Hex)
		if e != nil || output.LockingScript == nil || !bytes.Equal(script, *output.LockingScript) {
			return ev, fmt.Errorf("output script mismatch")
		}
		live = true
	}
	if err = s.stable(ctx, tip); err != nil {
		return ev, err
	}
	ev.Classification = FullySpent
	ev.Reason = "canonical inclusion and all outputs absent at pinned tip"
	if live {
		ev.Classification = Live
		ev.Reason = "confirmed output remains live"
	}
	return ev, nil
}
func checkedTransaction(id, raw string) (*bt.Tx, error) {
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid raw transaction hex")
	}
	tx, err := ReadBoundedTransaction(bytes.NewReader(b))
	if err != nil || tx == nil || tx.TxID() != id || !bytes.Equal(tx.Bytes(), b) || len(tx.Outputs) == 0 {
		return nil, fmt.Errorf("raw transaction identity or encoding mismatch")
	}
	return tx, nil
}

// RPCOutput is a confirmed output in the trusted RPC view, excluding mempool
// spends. A nil output only denotes absence; it never establishes confirmation.
type RPCOutput struct {
	Satoshis      uint64
	Script        []byte
	Confirmations int64
}

func rpcAmount(value json.Number) (uint64, error) {
	s := string(value)
	if len(s) == 0 || len(s) > 32 {
		return 0, fmt.Errorf("invalid output amount")
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(s[i+1:])
		if err != nil || exp < -20 || exp > 20 {
			return 0, fmt.Errorf("invalid output amount exponent")
		}
	}
	amount, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("invalid output amount")
	}
	amount.Mul(amount, big.NewRat(100000000, 1))
	if !amount.IsInt() || !amount.Num().IsUint64() {
		return 0, fmt.Errorf("invalid output amount precision or range")
	}
	return amount.Num().Uint64(), nil
}

func (s *RPCSource) Output(ctx context.Context, id string, vout uint32, tip Tip) (*RPCOutput, error) {
	if _, err := strictHash(id); err != nil {
		return nil, err
	}
	if err := s.stable(ctx, tip); err != nil {
		return nil, err
	}
	var state *struct {
		BestBlock     string      `json:"bestblock"`
		Confirmations int64       `json:"confirmations"`
		Value         json.Number `json:"value"`
		Script        struct {
			Hex *string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := s.call(ctx, "gettxout", []any{id, vout, false}, &state); err != nil {
		return nil, err
	}
	var output *RPCOutput
	if state != nil {
		if state.BestBlock != tip.Hash || state.Confirmations <= 0 || state.Script.Hex == nil {
			return nil, fmt.Errorf("inconsistent output state")
		}
		amount, err := rpcAmount(state.Value)
		if err != nil {
			return nil, err
		}
		script, err := hex.DecodeString(*state.Script.Hex)
		if err != nil {
			return nil, fmt.Errorf("invalid output script")
		}
		output = &RPCOutput{Satoshis: amount, Script: script, Confirmations: state.Confirmations}
	}
	if err := s.stable(ctx, tip); err != nil {
		return nil, err
	}
	return output, nil
}
func (s *RPCSource) Unspent(ctx context.Context, id string, vout uint32, tip Tip) (bool, error) {
	output, err := s.Output(ctx, id, vout, tip)
	return output != nil, err
}

// Reject duplicate keys at every nesting level: last-key-wins decoding can
// silently turn a contradictory RPC envelope into deletion authorization.
func strictRPCJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("RPC JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("malformed RPC JSON")
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				key, e := decoder.Token()
				if e != nil {
					return fmt.Errorf("malformed RPC object")
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("malformed RPC key")
				}
				if _, exists := keys[name]; exists {
					return fmt.Errorf("duplicate RPC JSON key")
				}
				keys[name] = struct{}{}
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			close, e := decoder.Token()
			if e != nil || close != json.Delim('}') {
				return fmt.Errorf("malformed RPC object")
			}
		case '[':
			for decoder.More() {
				if e := value(depth + 1); e != nil {
					return e
				}
			}
			close, e := decoder.Token()
			if e != nil || close != json.Delim(']') {
				return fmt.Errorf("malformed RPC array")
			}
		default:
			return fmt.Errorf("malformed RPC delimiter")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing RPC JSON")
	}
	return nil
}
