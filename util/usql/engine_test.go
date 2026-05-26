package usql

import "testing"

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
