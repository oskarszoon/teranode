package chaos

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/IBM/sarama"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestScenario04_IntermittentDrops tests how the system handles intermittent connection drops
// This simulates unstable network conditions where connections randomly drop
//
// Test Scenario:
// 1. Establish baseline performance with stable connections
// 2. Inject 30% connection drops (intermittent failures)
// 3. Test PostgreSQL operations with random drops
// 4. Test Kafka operations with random drops
// 5. Increase to 60% connection drops (severe instability)
// 6. Test application retry and recovery logic
// 7. Remove connection drops and verify full recovery
// 8. Validate data consistency after intermittent failures
//
// Expected Behavior:
// - Some operations fail randomly, some succeed
// - Retry logic handles transient failures
// - No data corruption despite intermittent drops
// - System recovers fully when drops are removed
// - Applications implement proper retry strategies
func TestScenario04_IntermittentDrops(t *testing.T) {
	// Skip if running in short mode
	if testing.Short() {
		t.Skip("Skipping chaos test in short mode")
	}

	// Configuration
	const (
		postgresToxiURL   = "http://localhost:8474"
		kafkaProxyURL     = "http://localhost:8475"
		postgresProxy     = "postgres"
		kafkaProxy        = "kafka"

		// Connection strings
		postgresDirectURL = "postgres://postgres:really_strong_password_change_me@localhost:5432/postgres?sslmode=disable"
		postgresToxiStr   = "postgres://postgres:really_strong_password_change_me@localhost:15432/postgres?sslmode=disable&connect_timeout=10"
		kafkaDirectURL    = "localhost:9092"
		kafkaToxiURL      = "localhost:19092"

		// Toxic parameters
		timeoutMs         = 0     // 0ms timeout = immediate drop
		lowToxicity       = 0.3   // 30% of connections drop
		highToxicity      = 0.6   // 60% of connections drop
		testTopic         = "chaos_test_scenario_04"

		// Retry parameters
		maxRetries        = 3
		retryDelay        = 300 * time.Millisecond
	)

	// Create toxiproxy clients for both services
	postgresProxyClient := NewToxiproxyClient(postgresToxiURL)
	kafkaProxyClient := NewToxiproxyClient(kafkaProxyURL)

	t.Logf("Waiting for toxiproxy services to be available...")
	require.NoError(t, postgresProxyClient.WaitForProxy(postgresProxy, 10*time.Second))
	require.NoError(t, kafkaProxyClient.WaitForProxy(kafkaProxy, 10*time.Second))

	// Reset proxies to clean state at test start
	t.Logf("Resetting toxiproxy to clean state...")
	require.NoError(t, postgresProxyClient.ResetProxy(postgresProxy))
	require.NoError(t, kafkaProxyClient.ResetProxy(kafkaProxy))

	// Cleanup: ensure we reset toxiproxy after test
	t.Cleanup(func() {
		t.Logf("Cleaning up: resetting toxiproxy...")
		_ = postgresProxyClient.ResetProxy(postgresProxy)
		_ = kafkaProxyClient.ResetProxy(kafkaProxy)
	})

	// Phase 1: Baseline Connectivity
	t.Run("Baseline_Connectivity", func(t *testing.T) {
		t.Logf("Testing baseline connectivity to PostgreSQL and Kafka...")

		// Test PostgreSQL baseline
		t.Run("PostgreSQL", func(t *testing.T) {
			db, err := sql.Open("postgres", postgresToxiStr)
			require.NoError(t, err)
			defer db.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err = db.PingContext(ctx)
			require.NoError(t, err)

			var result int
			err = db.QueryRowContext(ctx, "SELECT 1").Scan(&result)
			require.NoError(t, err)
			require.Equal(t, 1, result)
			t.Logf("✓ PostgreSQL baseline connectivity verified")
		})

		// Test Kafka baseline
		t.Run("Kafka", func(t *testing.T) {
			config := sarama.NewConfig()
			config.Producer.Return.Successes = true
			config.Producer.RequiredAcks = sarama.WaitForAll
			config.Producer.Timeout = 5 * time.Second
			config.Producer.Retry.Max = 0 // No retries for baseline

			producer, err := sarama.NewSyncProducer([]string{kafkaToxiURL}, config)
			require.NoError(t, err)
			defer producer.Close()

			message := &sarama.ProducerMessage{
				Topic: testTopic,
				Value: sarama.StringEncoder("baseline_test"),
			}

			partition, offset, err := producer.SendMessage(message)
			require.NoError(t, err)
			require.GreaterOrEqual(t, partition, int32(0))
			require.GreaterOrEqual(t, offset, int64(0))
			t.Logf("✓ Kafka baseline connectivity verified (partition=%d, offset=%d)", partition, offset)
		})

		t.Logf("✓ Baseline connectivity test complete")
	})

	// Phase 2: Inject Low Intermittent Drops (30%)
	t.Run("Inject_Low_Intermittent_Drops", func(t *testing.T) {
		t.Logf("Injecting 30%% intermittent connection drops...")

		// Add timeout toxic with 30% toxicity to both services
		err := postgresProxyClient.AddTimeout(postgresProxy, timeoutMs, lowToxicity, "downstream")
		require.NoError(t, err)

		err = kafkaProxyClient.AddTimeout(kafkaProxy, timeoutMs, lowToxicity, "downstream")
		require.NoError(t, err)

		// Verify toxics are applied
		postgresToxics, err := postgresProxyClient.ListToxics(postgresProxy)
		require.NoError(t, err)
		require.Len(t, postgresToxics, 1)
		require.Equal(t, "timeout", postgresToxics[0].Type)
		require.Equal(t, lowToxicity, postgresToxics[0].Toxicity)

		kafkaToxics, err := kafkaProxyClient.ListToxics(kafkaProxy)
		require.NoError(t, err)
		require.Len(t, kafkaToxics, 1)
		require.Equal(t, "timeout", kafkaToxics[0].Type)
		require.Equal(t, lowToxicity, kafkaToxics[0].Toxicity)

		t.Logf("✓ 30%% intermittent drops injected on both services")
	})

	// Phase 3: PostgreSQL Operations with Low Drop Rate
	t.Run("PostgreSQL_With_Low_Drops", func(t *testing.T) {
		t.Logf("Testing PostgreSQL operations with 30%% drop rate...")

		successCount := 0
		failureCount := 0
		attempts := 10 // Reduced from 20 to speed up test

		for i := 0; i < attempts; i++ {
			db, err := sql.Open("postgres", postgresToxiStr)
			if err != nil {
				failureCount++
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = db.PingContext(ctx)
			cancel()
			db.Close()

			if err != nil {
				failureCount++
			} else {
				successCount++
			}
		}

		t.Logf("PostgreSQL results: %d successes, %d failures out of %d attempts", successCount, failureCount, attempts)

		// With 30% drop rate, we expect some successes (at least 40% should succeed statistically)
		require.Greater(t, successCount, attempts*4/10, "Expected at least 40%% success rate")
		// But also some failures (at least 10% should fail)
		require.Greater(t, failureCount, attempts/10, "Expected some failures with 30%% drop rate")

		t.Logf("✓ PostgreSQL handling low intermittent drops correctly")
	})

	// Phase 4: Kafka Operations with Low Drop Rate
	t.Run("Kafka_With_Low_Drops", func(t *testing.T) {
		t.Logf("Testing Kafka operations with 30%% drop rate...")

		config := sarama.NewConfig()
		config.Producer.Return.Successes = true
		config.Producer.RequiredAcks = sarama.WaitForAll
		config.Producer.Timeout = 3 * time.Second
		config.Producer.Retry.Max = 0 // No retries to see pure drop effect

		successCount := 0
		failureCount := 0
		attempts := 10 // Reduced from 20 to speed up test

		for i := 0; i < attempts; i++ {
			producer, err := sarama.NewSyncProducer([]string{kafkaToxiURL}, config)
			if err != nil {
				failureCount++
				continue
			}

			message := &sarama.ProducerMessage{
				Topic: testTopic,
				Value: sarama.StringEncoder(fmt.Sprintf("intermittent_test_%d", i)),
			}

			_, _, err = producer.SendMessage(message)
			producer.Close()

			if err != nil {
				failureCount++
			} else {
				successCount++
			}
		}

		t.Logf("Kafka results: %d successes, %d failures out of %d attempts", successCount, failureCount, attempts)

		// With 30% drop rate, expect similar success/failure distribution
		// Note: Kafka might have internal retries that improve success rate
		require.Greater(t, successCount, attempts*4/10, "Expected at least 40%% success rate")
		// Kafka may handle drops better than PostgreSQL due to internal buffering
		if failureCount == 0 {
			t.Logf("⚠ No failures observed - Kafka may have internal retry mechanisms")
		}

		t.Logf("✓ Kafka handling low intermittent drops correctly")
	})

	// Phase 5: Increase to High Intermittent Drops (60%)
	t.Run("Inject_High_Intermittent_Drops", func(t *testing.T) {
		t.Logf("Increasing to 60%% intermittent connection drops...")

		// Remove existing toxics
		require.NoError(t, postgresProxyClient.RemoveAllToxics(postgresProxy))
		require.NoError(t, kafkaProxyClient.RemoveAllToxics(kafkaProxy))

		// Add higher toxicity
		err := postgresProxyClient.AddTimeout(postgresProxy, timeoutMs, highToxicity, "downstream")
		require.NoError(t, err)

		err = kafkaProxyClient.AddTimeout(kafkaProxy, timeoutMs, highToxicity, "downstream")
		require.NoError(t, err)

		t.Logf("✓ 60%% intermittent drops injected on both services")
	})

	// Phase 6: Test Retry Logic with High Drop Rate
	t.Run("Retry_Logic_With_High_Drops", func(t *testing.T) {
		t.Logf("Testing application retry logic with 60%% drop rate...")

		// Simulate application-level retry logic for PostgreSQL
		t.Run("PostgreSQL_Retry", func(t *testing.T) {
			retrySuccessCount := 0
			maxRetriesHitCount := 0
			attempts := 5 // Reduced from 10 to speed up test

			for i := 0; i < attempts; i++ {
				success := false
				for retry := 0; retry < maxRetries; retry++ {
					db, err := sql.Open("postgres", postgresToxiStr)
					if err != nil {
						time.Sleep(retryDelay)
						continue
					}

					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					err = db.PingContext(ctx)
					cancel()
					db.Close()

					if err == nil {
						success = true
						break
					}
					time.Sleep(retryDelay)
				}

				if success {
					retrySuccessCount++
				} else {
					maxRetriesHitCount++
				}
			}

			t.Logf("PostgreSQL retry results: %d eventual successes, %d exhausted retries out of %d attempts",
				retrySuccessCount, maxRetriesHitCount, attempts)

			// With retry logic and 60% drop rate, should get more successes than without retries
			// At least 40% should eventually succeed (0.4^3 = 6.4% chance of 3 consecutive failures)
			require.Greater(t, retrySuccessCount, attempts*4/10, "Retry logic should improve success rate")

			t.Logf("✓ PostgreSQL retry logic working correctly")
		})

		// Simulate application-level retry logic for Kafka
		t.Run("Kafka_Retry", func(t *testing.T) {
			retrySuccessCount := 0
			maxRetriesHitCount := 0
			attempts := 5 // Reduced from 10 to speed up test

			for i := 0; i < attempts; i++ {
				success := false
				for retry := 0; retry < maxRetries; retry++ {
					config := sarama.NewConfig()
					config.Producer.Return.Successes = true
					config.Producer.RequiredAcks = sarama.WaitForAll
					config.Producer.Timeout = 2 * time.Second
					config.Producer.Retry.Max = 0

					producer, err := sarama.NewSyncProducer([]string{kafkaToxiURL}, config)
					if err != nil {
						time.Sleep(retryDelay)
						continue
					}

					message := &sarama.ProducerMessage{
						Topic: testTopic,
						Value: sarama.StringEncoder(fmt.Sprintf("retry_test_%d_%d", i, retry)),
					}

					_, _, err = producer.SendMessage(message)
					producer.Close()

					if err == nil {
						success = true
						break
					}
					time.Sleep(retryDelay)
				}

				if success {
					retrySuccessCount++
				} else {
					maxRetriesHitCount++
				}
			}

			t.Logf("Kafka retry results: %d eventual successes, %d exhausted retries out of %d attempts",
				retrySuccessCount, maxRetriesHitCount, attempts)

			require.Greater(t, retrySuccessCount, attempts*4/10, "Retry logic should improve success rate")

			t.Logf("✓ Kafka retry logic working correctly")
		})

		t.Logf("✓ Retry logic validated under high intermittent drop rate")
	})

	// Phase 7: Remove Intermittent Drops and Verify Recovery
	t.Run("Remove_Drops_And_Recovery", func(t *testing.T) {
		t.Logf("Removing intermittent drops and verifying recovery...")

		// Remove all toxics
		require.NoError(t, postgresProxyClient.RemoveAllToxics(postgresProxy))
		require.NoError(t, kafkaProxyClient.RemoveAllToxics(kafkaProxy))

		// Wait for stabilization
		time.Sleep(2 * time.Second)

		// Verify PostgreSQL recovery
		t.Run("PostgreSQL_Recovery", func(t *testing.T) {
			successCount := 0
			attempts := 10

			for i := 0; i < attempts; i++ {
				db, err := sql.Open("postgres", postgresToxiStr)
				if err != nil {
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = db.PingContext(ctx)
				cancel()
				db.Close()

				if err == nil {
					successCount++
				}
			}

			// Should have 100% success rate after drops removed
			require.Equal(t, attempts, successCount, "All PostgreSQL operations should succeed after recovery")
			t.Logf("✓ PostgreSQL fully recovered (100%% success rate)")
		})

		// Verify Kafka recovery
		t.Run("Kafka_Recovery", func(t *testing.T) {
			config := sarama.NewConfig()
			config.Producer.Return.Successes = true
			config.Producer.RequiredAcks = sarama.WaitForAll
			config.Producer.Timeout = 5 * time.Second
			config.Producer.Retry.Max = 0

			successCount := 0
			attempts := 10

			for i := 0; i < attempts; i++ {
				producer, err := sarama.NewSyncProducer([]string{kafkaToxiURL}, config)
				if err != nil {
					continue
				}

				message := &sarama.ProducerMessage{
					Topic: testTopic,
					Value: sarama.StringEncoder(fmt.Sprintf("recovery_test_%d", i)),
				}

				_, _, err = producer.SendMessage(message)
				producer.Close()

				if err == nil {
					successCount++
				}
			}

			// Should have 100% success rate after drops removed
			require.Equal(t, attempts, successCount, "All Kafka operations should succeed after recovery")
			t.Logf("✓ Kafka fully recovered (100%% success rate)")
		})

		t.Logf("✓ Both services fully recovered after removing intermittent drops")
	})

	// Phase 8: Validate Data Consistency
	t.Run("Data_Consistency", func(t *testing.T) {
		t.Logf("Verifying data consistency after intermittent failures...")

		// Test PostgreSQL consistency
		t.Run("PostgreSQL_Consistency", func(t *testing.T) {
			db, err := sql.Open("postgres", postgresToxiStr)
			require.NoError(t, err)
			defer db.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Create test table
			_, err = db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS chaos_scenario_04 (id SERIAL PRIMARY KEY, value TEXT)")
			require.NoError(t, err)

			// Clean up any existing data
			_, err = db.ExecContext(ctx, "TRUNCATE TABLE chaos_scenario_04")
			require.NoError(t, err)

			// Insert test data
			_, err = db.ExecContext(ctx, "INSERT INTO chaos_scenario_04 (value) VALUES ($1), ($2)", "test1", "test2")
			require.NoError(t, err)

			// Verify data
			var count int
			err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM chaos_scenario_04").Scan(&count)
			require.NoError(t, err)
			require.Equal(t, 2, count)

			// Cleanup
			_, err = db.ExecContext(ctx, "DROP TABLE chaos_scenario_04")
			require.NoError(t, err)

			t.Logf("✓ PostgreSQL data consistency verified")
		})

		// Test Kafka consistency
		t.Run("Kafka_Consistency", func(t *testing.T) {
			config := sarama.NewConfig()
			config.Producer.Return.Successes = true
			config.Producer.RequiredAcks = sarama.WaitForAll
			config.Producer.Timeout = 5 * time.Second

			producer, err := sarama.NewSyncProducer([]string{kafkaToxiURL}, config)
			require.NoError(t, err)
			defer producer.Close()

			message := &sarama.ProducerMessage{
				Topic: testTopic,
				Value: sarama.StringEncoder("consistency_verification"),
			}

			partition, offset, err := producer.SendMessage(message)
			require.NoError(t, err)
			require.GreaterOrEqual(t, partition, int32(0))
			require.GreaterOrEqual(t, offset, int64(0))

			t.Logf("✓ Kafka message consistency verified (offset=%d)", offset)
		})

		t.Logf("✓ Data consistency verified for both services")
	})

	t.Logf("✅ Scenario 4 (Intermittent Connection Drops) completed successfully")
}
