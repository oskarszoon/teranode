//go:build cockroach

// Package crdb provides a build-tagged CockroachDB testcontainer helper.
// It lives in a sub-package (separate from test/testcontainers) so that
// stores can import it without dragging in the daemon dependency that the
// parent package carries — daemon → stores/utxo/factory → stores/utxo/sql
// would create an import cycle otherwise.
package crdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartCockroach boots a single-node insecure CockroachDB container, creates
// a fresh test database, and returns a postgres-wire connection URL. The
// container is automatically torn down via t.Cleanup.
//
// Pinned to v24.1 to keep the test stable across CRDB releases. Update
// deliberately when validating against a newer version.
func StartCockroach(t *testing.T, ctx context.Context, dbName string) (url string) {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        "cockroachdb/cockroach:v24.1.13",
		ExposedPorts: []string{"26257/tcp"},
		// CRDB v24.1 rejects "--listen-addr=0.0.0.0:..." with: hostname of
		// listen_addr must be "127.0.0.1" or "localhost". An empty hostname
		// (":26257") binds all interfaces and is what CRDB itself recommends
		// for containerised single-node test setups; --advertise-addr makes
		// the node usable from outside the container via the mapped port.
		Cmd: []string{
			"start-single-node",
			"--insecure",
			"--listen-addr=:26257",
			"--advertise-addr=localhost:26257",
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("26257/tcp"),
			wait.ForLog("nodeID:"),
		),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "26257")
	require.NoError(t, err)

	rootURL := fmt.Sprintf("postgres://root@%s:%s/defaultdb?sslmode=disable", host, port.Port())

	// Create the test database from the default connection.
	createDB(t, ctx, rootURL, dbName)

	return fmt.Sprintf("postgres://root@%s:%s/%s?sslmode=disable", host, port.Port(), dbName)
}

func createDB(t *testing.T, ctx context.Context, rootURL, dbName string) {
	t.Helper()
	// Open a one-shot connection via database/sql + pgx stdlib driver.
	// Avoid importing usql to keep this helper self-contained.
	db, err := sql.Open("pgx", rootURL)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", dbName))
	require.NoError(t, err)
}
