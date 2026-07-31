// Package subtreevalidation provides functionality for validating subtrees in a blockchain context.
// It handles the validation of transaction subtrees, manages transaction metadata caching,
// and interfaces with blockchain and validation services.
package subtreevalidation

import (
	"sync"

	"github.com/bsv-blockchain/teranode/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for monitoring subtree validation service performance and health.
//
// These metrics provide comprehensive observability into the subtree validation service,
// enabling monitoring of operation durations, success/failure rates, retry patterns,
// and overall service health. The metrics are automatically registered with Prometheus
// and can be scraped by monitoring systems.
//
// Metric Categories:
//   - Health metrics: Service availability and readiness checks
//   - Operation metrics: Duration and success rates of validation operations
//   - Retry metrics: Tracking of retry attempts and patterns
//   - Handler metrics: Performance of individual API handlers
//   - Processing metrics: Internal processing step performance
//
// All histogram metrics use standard duration buckets appropriate for blockchain
// validation operations, typically ranging from milliseconds to seconds.
var (
	// prometheusHealth tracks the duration of health check operations.
	// This histogram measures how long health checks take to complete,
	// which is important for monitoring service responsiveness.
	prometheusHealth prometheus.Histogram

	// prometheusSubtreeValidationCheckSubtree tracks the duration of subtree existence checks.
	// This histogram measures the time taken to verify if a subtree already exists
	// in storage, which is a common optimization operation.
	prometheusSubtreeValidationCheckSubtree prometheus.Histogram

	// prometheusSubtreeValidationValidateSubtree tracks the duration of complete subtree validation operations.
	// This is the primary metric for monitoring the core validation functionality,
	// measuring end-to-end validation time including all sub-operations.
	prometheusSubtreeValidationValidateSubtree prometheus.Histogram

	// prometheusSubtreeValidationValidateSubtreeRetry counts retry attempts during subtree validation.
	// This counter tracks how often validation operations need to be retried,
	// which helps identify reliability issues or transient failures.
	prometheusSubtreeValidationValidateSubtreeRetry prometheus.Counter

	// prometheusSubtreeValidationValidateSubtreeHandler tracks the duration of validation API handler operations.
	// This histogram measures the time spent in the gRPC handler layer,
	// including request processing and response generation.
	prometheusSubtreeValidationValidateSubtreeHandler prometheus.Histogram

	// prometheusSubtreeValidationValidateSubtreeDuration tracks detailed validation processing time.
	// This histogram provides granular timing information for the internal
	// validation logic, separate from handler overhead.
	prometheusSubtreeValidationValidateSubtreeDuration prometheus.Histogram

	// prometheusSubtreeValidationLevelsPerBlock records how many dependency levels a
	// block's unseen transactions were organised into by selectPrepareTxsPerLevel.
	//
	// This is the number that decides the cost of a block on this path. Levels are
	// processed serially with a barrier each (check_block_subtrees.go), and a level
	// costs roughly a fixed set of store round trips regardless of how many
	// transactions it holds — so total cost tracks level COUNT, not transaction count.
	// It was previously available only at Debugf, which meant the depth of the two
	// slow mainnet blocks in #1379 had to be reconstructed from a third-party API.
	prometheusSubtreeValidationLevelsPerBlock prometheus.Histogram

	// prometheusSubtreeValidationLevelDuration tracks the wall time of a single
	// dependency level: parent prefetch, then the bounded-parallel validation of every
	// transaction at that level, up to the barrier.
	//
	// Per-level timing was Debugf-only, so diagnosing #1379 required deriving a
	// per-level average by dividing a total by a level count obtained elsewhere.
	prometheusSubtreeValidationLevelDuration prometheus.Histogram

	// prometheusSubtreeValidationPrefetchLevelParents tracks the duration of the
	// per-level bulk parent read. It is one of the serialised round trips on a level's
	// critical path and had no instrumentation at all.
	prometheusSubtreeValidationPrefetchLevelParents prometheus.Histogram

	// prometheusSubtreeValidationBlessMissingTransaction tracks the duration of bless missing transaction operations.
	// This histogram measures the time taken to handle missing transactions,
	// which is an important aspect of subtree validation.
	prometheusSubtreeValidationBlessMissingTransaction prometheus.Histogram

	// prometheusSubtreeValidationSetTXMetaCacheKafkaBatch tracks the duration of processing one
	// SetCacheMulti batch from a Kafka message. One observation per Kafka message that contained
	// at least one ADD entry, regardless of how many entries the batch held.
	prometheusSubtreeValidationSetTXMetaCacheKafkaBatch prometheus.Histogram

	// prometheusSubtreeValidationSetTXMetaCacheKafkaCount counts individual ADD entries
	// processed from Kafka (attempts, not successes — failures are tracked separately by
	// prometheusSubtreeValidationSetTXMetaCacheKafkaErrors, matching the pre-batching
	// behaviour where the histogram was observed regardless of cache write outcome).
	// Pre-batching this was the auto-emitted "set_tx_meta_cache_kafka_count" of the
	// duration histogram; the batched dispatch path emits one histogram observation per
	// Kafka message, so the per-entry rate moved to this explicit counter to keep
	// `rate(set_tx_meta_cache_kafka_count[…])` reading entries/sec.
	prometheusSubtreeValidationSetTXMetaCacheKafkaCount prometheus.Counter

	// prometheusSubtreeValidationDelTXMetaCacheKafka tracks the duration of deleting tx meta cache from kafka operations.
	// This histogram measures the time taken to remove transaction metadata from the cache
	// based on Kafka messages, which is an important maintenance operation.
	prometheusSubtreeValidationDelTXMetaCacheKafka prometheus.Histogram

	// prometheusSubtreeValidationSetTXMetaCacheKafkaErrors counts errors setting tx meta cache from kafka operations.
	// This counter tracks how often errors occur when updating the transaction metadata cache
	// from Kafka messages, which helps identify issues with the cache or Kafka connection.
	prometheusSubtreeValidationSetTXMetaCacheKafkaErrors prometheus.Counter

	// prometheusSubtreeKafkaMalformed counts malformed Kafka subtree messages that were dropped,
	// labelled by the reason for the drop. Returning nil from the consumer for permanently-malformed
	// messages preserves Kafka offset progression (avoids a poison-pill retry loop), but ops still
	// need a signal so they can detect malformed-message bursts that would otherwise be invisible.
	// Reason labels: nil_message, too_short, unmarshal_failure, bad_hash, bad_url.
	prometheusSubtreeKafkaMalformed *prometheus.CounterVec
)

var (
	prometheusMetricsInitOnce sync.Once
)

func InitPrometheusMetrics() {
	prometheusMetricsInitOnce.Do(_initPrometheusMetrics)
}

func _initPrometheusMetrics() {
	prometheusHealth = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "health",
			Help:      "Histogram of calls to health endpoint",
			Buckets:   util.MetricsBucketsMilliLongSeconds,
		},
	)

	prometheusSubtreeValidationCheckSubtree = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "check_subtree",
			Help:      "Duration of calls to checkSubtree endpoint",
			Buckets:   util.MetricsBucketsMilliLongSeconds,
		},
	)

	prometheusSubtreeValidationValidateSubtree = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "validate_subtree",
			Help:      "Histogram of subtrees validated",
			Buckets:   util.MetricsBucketsMilliLongSeconds,
		},
	)

	prometheusSubtreeValidationValidateSubtreeRetry = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "validate_subtree_retry",
			Help:      "Number of retries when subtrees validated",
		},
	)

	prometheusSubtreeValidationValidateSubtreeHandler = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "validate_subtree_handler",
			Help:      "Duration of subtree handler",
			Buckets:   util.MetricsBucketsMilliLongSeconds,
		},
	)

	prometheusSubtreeValidationValidateSubtreeDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "validate_subtree_duration",
			Help:      "Duration of validate subtree",
			Buckets:   util.MetricsBucketsMilliLongSeconds,
		},
	)

	prometheusSubtreeValidationLevelsPerBlock = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "levels_per_block",
			Help:      "Number of dependency levels the unseen transactions of a block were organised into",
			// Level depth, not a duration: powers of two up to 4096 so both a flat
			// block (1) and a pathologically deep one land in distinct buckets.
			Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096},
		},
	)

	prometheusSubtreeValidationLevelDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "level_duration",
			Help:      "Duration of processing a single dependency level, including parent prefetch",
			Buckets:   util.MetricsBucketsMilliSeconds,
		},
	)

	prometheusSubtreeValidationPrefetchLevelParents = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "prefetch_level_parents",
			Help:      "Duration of the per-level bulk parent read",
			Buckets:   util.MetricsBucketsMilliSeconds,
		},
	)

	prometheusSubtreeValidationBlessMissingTransaction = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "bless_missing_transaction",
			Help:      "Duration of bless missing transaction",
			Buckets:   util.MetricsBucketsMilliSeconds,
		},
	)

	prometheusSubtreeValidationSetTXMetaCacheKafkaBatch = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "set_tx_meta_cache_kafka_batch",
			Help:      "Duration of one SetCacheMulti batch from a Kafka txmeta message (per Kafka message, not per entry)",
			Buckets:   util.MetricsBucketsMicroSeconds,
		},
	)

	prometheusSubtreeValidationSetTXMetaCacheKafkaCount = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "set_tx_meta_cache_kafka_count",
			Help:      "Number of ADD entries processed from Kafka (per-entry; attempts, not successes — failures separately in *_errors)",
		},
	)

	prometheusSubtreeValidationDelTXMetaCacheKafka = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "del_tx_meta_cache_kafka",
			Help:      "Duration of deleting tx meta cache from kafka",
			Buckets:   util.MetricsBucketsMicroSeconds,
		},
	)

	prometheusSubtreeValidationSetTXMetaCacheKafkaErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "set_tx_meta_cache_kafka_errors",
			Help:      "Number of errors setting tx meta cache from kafka",
		},
	)

	prometheusSubtreeKafkaMalformed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "teranode",
			Subsystem: "subtreevalidation",
			Name:      "kafka_malformed_messages_total",
			Help:      "Number of malformed Kafka subtree messages dropped, by reason",
		},
		[]string{"reason"},
	)
}
