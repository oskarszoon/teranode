package blockassembly

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockassembly/blockassembly_api"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRecoveryStreamCompleteness(t *testing.T) {
	txid := chainhash.Hash{1}.String()
	state := &blockassembly_api.RecoveryStateMessage{ProcessId: "process", TipHash: chainhash.Hash{2}.String()}
	open := &blockassembly_api.RecoveryPage{State: state, Count: 1}
	data := &blockassembly_api.RecoveryPage{Txids: []string{txid}}
	close := &blockassembly_api.RecoveryPage{State: state, Count: 1, Complete: true}
	for _, tc := range []struct {
		name    string
		pages   []*blockassembly_api.RecoveryPage
		success bool
	}{
		{"complete", []*blockassembly_api.RecoveryPage{open, data, close}, true},
		{"truncated", []*blockassembly_api.RecoveryPage{open, data}, false},
		{"missing transaction", []*blockassembly_api.RecoveryPage{open, close}, false},
		{"extra transaction", []*blockassembly_api.RecoveryPage{open, data, data, close}, false},
		{"missing opening", []*blockassembly_api.RecoveryPage{data, close}, false},
		{"trailing data", []*blockassembly_api.RecoveryPage{open, data, close, data}, false},
		{"identity changed", []*blockassembly_api.RecoveryPage{open, data, {State: &blockassembly_api.RecoveryStateMessage{ProcessId: "restart", TipHash: state.TipHash}, Count: 1, Complete: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursor := 0
			var got []string
			_, err := consumeRecoveryStream(func() (*blockassembly_api.RecoveryPage, error) {
				if cursor == len(tc.pages) {
					return nil, io.EOF
				}
				p := tc.pages[cursor]
				cursor++
				return p, nil
			}, false, func(txid string) error { got = append(got, txid); return nil })
			if tc.success {
				require.NoError(t, err)
				require.Equal(t, []string{txid}, got)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRecoveryRPCStreamsMultipleBoundedPages(t *testing.T) {
	initPrometheusMetrics()
	items := setupBlockAssemblyTest(t)
	b := items.blockAssembler
	setupBlockchainClient(t, items)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	go func() {
		for {
			select {
			case req := <-items.newSubtreeChan:
				if req.ErrChan != nil {
					select {
					case req.ErrChan <- nil:
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	require.NoError(t, b.startChannelListeners(ctx))
	t.Cleanup(func() { cancel(); b.wg.Wait() })
	const total = 2053
	nodes := make([]subtree.Node, total)
	inpoints := make([]*subtree.TxInpoints, total)
	for i := range nodes {
		nodes[i] = subtree.Node{Hash: chainhash.HashH([]byte(fmt.Sprint(i))), SizeInBytes: 1}
		inpoints[i] = &subtree.TxInpoints{ParentTxHashes: []chainhash.Hash{}}
	}
	require.True(t, b.AddTxBatchIfRoom(nodes, inpoints))
	require.Eventually(t, func() bool { return b.QueueLength() == 0 && b.TxCount() >= total }, 5*time.Second, 10*time.Millisecond)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	service := &BlockAssembly{blockAssembler: b, logger: b.logger, stats: b.stats, blockchainClient: b.blockchainClient, jobStore: ttlcache.New[chainhash.Hash, *subtreeprocessor.Job]()}
	blockassembly_api.RegisterBlockAssemblyAPIServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///recovery", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	api := blockassembly_api.NewBlockAssemblyAPIClient(conn)
	stream, err := api.RecoveryTransactions(ctx, &blockassembly_api.RecoveryTransactionsRequest{})
	require.NoError(t, err)
	pages := 0
	count := 0
	state, err := consumeRecoveryStream(func() (*blockassembly_api.RecoveryPage, error) {
		p, err := stream.Recv()
		if err == nil && len(p.Txids) > 0 {
			pages++
			require.LessOrEqual(t, len(p.Txids), recoveryPageSize)
		}
		return p, err
	}, false, func(id string) error { require.Equal(t, nodes[count].Hash.String(), id); count++; return nil })
	require.NoError(t, err)
	require.Equal(t, total, count)
	require.Equal(t, 3, pages)
	require.NotEmpty(t, state.ProcessID)
	client := &Client{client: api}
	var candidateCount int
	candidate, err := client.RecoveryTransactions(ctx, true, func(string) error { candidateCount++; return nil })
	require.NoError(t, err)
	require.Equal(t, 2051, candidateCount)
	id, err := chainhash.NewHashFromStr(candidate.CandidateID)
	require.NoError(t, err)
	job := service.jobStore.Get(*id)
	require.NotNil(t, job, "recovery candidate is the actual registered mining job")
	require.EqualValues(t, candidateCount, job.Value().MiningCandidate.NumTxs)
	require.NotEmpty(t, candidate.CandidateID)
	require.Equal(t, state.Tip, candidate.Tip)
	callbackErr := errors.NewError("stop consuming")
	_, err = client.RecoveryTransactions(ctx, false, func(string) error { return callbackErr })
	require.ErrorIs(t, err, callbackErr)
	current, err := client.RecoveryState(ctx)
	require.NoError(t, err, "cancelled stream releases both control-loop owners")
	require.Equal(t, state.ProcessID, current.ProcessID)
}
