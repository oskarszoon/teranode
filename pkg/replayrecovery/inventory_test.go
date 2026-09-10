package replayrecovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type censusFixture struct {
	records []InventoryRecord
	refs    []SpendReference
}

func (c censusFixture) Inventory(ctx context.Context, visit func(InventoryRecord) error) error {
	for _, r := range c.records {
		if err := visit(r); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (c censusFixture) SpendReferences(ctx context.Context, visit func(SpendReference) error) error {
	for _, r := range c.refs {
		if err := visit(r); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func allowRecovery(context.Context) error { return nil }

func TestInventoryIncludesMinedParentMissingChildAndDirtyFlags(t *testing.T) {
	parent, child, dirty := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	c := censusFixture{
		records: []InventoryRecord{
			{Record: Record{Key: []byte("parent"), Data: []byte("spent")}, TxID: parent, Master: true},
			{Record: Record{Key: []byte("dirty"), Data: []byte("locked")}, TxID: dirty, Master: true, Candidate: true, Reason: "locked record"},
		},
		refs: []SpendReference{{ParentKey: []byte("parent"), ParentTxID: parent, ChildTxID: child}},
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "manifest.sqlite")
	summary, err := Inventory(t.Context(), c, path, allowRecovery)
	require.NoError(t, err)
	require.Equal(t, int64(2), summary.Scanned)
	db, err := openPrivateDB(path, false, true)
	require.NoError(t, err)
	defer db.Close()
	var count int
	require.NoError(t, db.db.QueryRow("SELECT COUNT(*) FROM entries WHERE id IN (?,?)", child, dirty).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, db.db.QueryRow("SELECT COUNT(*) FROM entries WHERE id=?", parent).Scan(&count))
	require.Zero(t, count, "normally mined parent must not become a delete candidate")
	var data []byte
	require.NoError(t, db.db.QueryRow("SELECT record FROM inventory WHERE key=?", []byte("dirty")).Scan(&data))
	var rec Record
	require.NoError(t, json.Unmarshal(data, &rec))
	require.Equal(t, []byte("locked"), rec.Data)
}

func TestInventoryRecordsMissingPagesAndRefusesAbsentGuard(t *testing.T) {
	id := strings.Repeat("4", 64)
	c := censusFixture{records: []InventoryRecord{{Record: Record{Key: []byte("master")}, TxID: id, Master: true, ExpectedPages: 1}}}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	_, err := Inventory(t.Context(), c, filepath.Join(dir, "unguarded"), nil)
	require.Error(t, err)
	path := filepath.Join(dir, "manifest.sqlite")
	_, err = Inventory(t.Context(), c, path, allowRecovery)
	require.NoError(t, err)
	db, err := openPrivateDB(path, false, true)
	require.NoError(t, err)
	defer db.Close()
	var count int
	require.NoError(t, db.db.QueryRow("SELECT COUNT(*) FROM findings").Scan(&count))
	require.Positive(t, count)
}
