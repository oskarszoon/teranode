package usql

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestIsPostgresLike(t *testing.T) {
	cases := []struct {
		engine Engine
		want   bool
	}{
		{EnginePostgres, true},
		{EngineCockroach, true},
		{EngineSqlite, false},
		{EngineSqliteMemory, false},
		{Engine(""), false},
		{Engine("mysql"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.engine), func(t *testing.T) {
			if got := IsPostgresLike(tc.engine); got != tc.want {
				t.Fatalf("IsPostgresLike(%q) = %v, want %v", tc.engine, got, tc.want)
			}
		})
	}
}

func TestDetectEngine(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		want      Engine
		wantError bool
	}{
		{"postgres 15", "PostgreSQL 15.5 on x86_64-pc-linux-gnu, compiled by gcc", EnginePostgres, false},
		{"postgres 13", "PostgreSQL 13.10 (Debian 13.10-1.pgdg110+1)", EnginePostgres, false},
		{"cockroach 24.1 ccl", "CockroachDB CCL v24.1.0 (x86_64-pc-linux-gnu, built 2024/05/20)", EngineCockroach, false},
		{"cockroach 23.2", "CockroachDB CCL v23.2.5", EngineCockroach, false},
		{"unknown engine", "MariaDB 11.2.0", "", true},
		{"empty version", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mock.ExpectQuery("SELECT version\\(\\)").
				WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(tc.version))

			got, err := DetectEngine(context.Background(), WrapDB(db))
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDetectEngine_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT version\\(\\)").WillReturnError(context.DeadlineExceeded)

	_, err = DetectEngine(context.Background(), WrapDB(db))
	require.Error(t, err)
}

func TestVerifySchemaColumns(t *testing.T) {
	expected := map[string][]string{
		"transactions": {"id", "hash", "version"},
		"outputs":      {"transaction_id", "idx"},
	}

	t.Run("all present", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		mock.ExpectQuery("SELECT column_name").WithArgs("outputs").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("transaction_id").AddRow("idx"))
		mock.ExpectQuery("SELECT column_name").WithArgs("transactions").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").AddRow("hash").AddRow("version"))

		err = VerifySchemaColumns(context.Background(), db, EngineCockroach, expected)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("missing column reported", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		mock.ExpectQuery("SELECT column_name").WithArgs("outputs").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("transaction_id").AddRow("idx"))
		mock.ExpectQuery("SELECT column_name").WithArgs("transactions").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").AddRow("hash"))

		err = VerifySchemaColumns(context.Background(), db, EngineCockroach, expected)
		require.Error(t, err)
		require.Contains(t, err.Error(), "transactions: version")
	})

	t.Run("query error propagated", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		mock.ExpectQuery("SELECT column_name").WithArgs("outputs").WillReturnError(context.DeadlineExceeded)

		err = VerifySchemaColumns(context.Background(), db, EngineCockroach, expected)
		require.Error(t, err)
	})

	t.Run("multi-table drift in one error", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		mock.ExpectQuery("SELECT column_name").WithArgs("outputs").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("transaction_id"))
		mock.ExpectQuery("SELECT column_name").WithArgs("transactions").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id"))

		err = VerifySchemaColumns(context.Background(), db, EngineCockroach, expected)
		require.Error(t, err)
		require.Contains(t, err.Error(), "outputs: idx")
		require.Contains(t, err.Error(), "transactions: hash,version")
	})
}
