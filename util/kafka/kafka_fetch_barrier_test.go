package kafka

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// A real broker proves that a held subtree handler prevents the next fetch
// from being dispatched, rather than only testing the option's configuration.
func TestWaitForFetchHandlersBackpressuresBrokerPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	broker, cleanup := startRedpanda(t, ctx)
	defer cleanup()
	topic := fmt.Sprintf("fetch-barrier-%d", time.Now().UnixNano())
	topicURL, err := url.Parse(fmt.Sprintf("kafka://%s/%s?partitions=1&replication=1&replay=1", broker, topic))
	require.NoError(t, err)
	consumer, err := NewKafkaConsumerGroupFromURL(ulogger.TestLogger{}, topicURL, topic+"-group", true, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, consumer.Close()) })
	producer, err := NewKafkaProducerWithContext(ctx, topicURL, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, producer.Close()) }()
	handlerCtx, handlerCancel := context.WithCancel(ctx)
	defer handlerCancel()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	thirdEntered := make(chan struct{})
	thirdExited := make(chan struct{})
	consumer.Start(ctx, func(msg *KafkaMessage) error {
		switch string(msg.Key) {
		case "first":
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-handlerCtx.Done():
				return handlerCtx.Err()
			}
		case "second":
			close(secondEntered)
		case "third":
			close(thirdEntered)
			<-handlerCtx.Done()
			close(thirdExited)
			return handlerCtx.Err()
		}
		return nil
	}, WithLogErrorAndMoveOn(), WithWaitForFetchHandlers(handlerCancel))
	require.NoError(t, producer.Send([]byte("first"), []byte("v")))
	require.Eventually(t, func() bool {
		select {
		case <-firstEntered:
			return true
		default:
			return false
		}
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, producer.Send([]byte("second"), []byte("v")))
	require.Eventually(t, func() bool { return consumer.client.BufferedFetchRecords() > 0 }, 10*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool {
		select {
		case <-secondEntered:
			return true
		default:
			return false
		}
	}, 250*time.Millisecond, 10*time.Millisecond)
	close(releaseFirst)
	require.Eventually(t, func() bool {
		select {
		case <-secondEntered:
			return true
		default:
			return false
		}
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, producer.Send([]byte("third"), []byte("v")))
	require.Eventually(t, func() bool {
		select {
		case <-thirdEntered:
			return true
		default:
			return false
		}
	}, 10*time.Second, 10*time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- consumer.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("consumer Close blocked behind a live-parent authority retry")
	}
	require.Eventually(t, func() bool {
		select {
		case <-thirdExited:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}
