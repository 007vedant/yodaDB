package catalog_test

import (
	"errors"
	"testing"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/types"
)

func TestCreateTableAssignsStableIDs(t *testing.T) {
	c := catalog.New()
	users, err := c.CreateTable("users", []catalog.ColumnSpec{
		{Name: "id", Type: types.Int64Kind, PrimaryKey: true},
		{Name: "name", Type: types.TextKind, NotNull: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := c.CreateTable("logs", []catalog.ColumnSpec{{Name: "message", Type: types.TextKind}})
	if err != nil {
		t.Fatal(err)
	}
	if users.ID != 1 || logs.ID != 2 {
		t.Fatalf("table IDs = %d, %d; want 1, 2", users.ID, logs.ID)
	}
	if users.Columns[0].ID != 1 || users.Columns[1].ID != 2 || logs.Columns[0].ID != 3 {
		t.Fatalf("column IDs are not monotonic: %#v, %#v", users.Columns, logs.Columns)
	}
	if users.PrimaryKeyIndex != 0 || !users.Columns[0].NotNull {
		t.Fatalf("primary key metadata = %#v", users)
	}
}

func TestFailedCreateDoesNotConsumeIDs(t *testing.T) {
	c := catalog.New()
	_, err := c.CreateTable("bad", []catalog.ColumnSpec{
		{Name: "id", Type: types.Int64Kind},
		{Name: "id", Type: types.Int64Kind},
	})
	if !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate-column error = %v, want constraint", err)
	}
	table, err := c.CreateTable("good", []catalog.ColumnSpec{{Name: "id", Type: types.Int64Kind}})
	if err != nil {
		t.Fatal(err)
	}
	if table.ID != 1 || table.Columns[0].ID != 1 {
		t.Fatalf("failed create consumed IDs: %#v", table)
	}
}

func TestLookupReturnsDescriptorCopy(t *testing.T) {
	c := catalog.New()
	_, err := c.CreateTable("users", []catalog.ColumnSpec{{Name: "id", Type: types.Int64Kind}})
	if err != nil {
		t.Fatal(err)
	}
	first, ok := c.LookupTable("users")
	if !ok {
		t.Fatal("users table not found")
	}
	first.Columns[0].Name = "mutated"
	second, _ := c.LookupTable("users")
	if second.Columns[0].Name != "id" {
		t.Fatalf("catalog descriptor mutated through returned slice: %#v", second)
	}
}

func TestCreateRejectsDuplicateTableAndMultiplePrimaryKeys(t *testing.T) {
	c := catalog.New()
	_, err := c.CreateTable("users", []catalog.ColumnSpec{{Name: "id", Type: types.Int64Kind}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTable("users", []catalog.ColumnSpec{{Name: "other", Type: types.Int64Kind}}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate table error = %v, want constraint", err)
	}
	if _, err := c.CreateTable("two_keys", []catalog.ColumnSpec{
		{Name: "a", Type: types.Int64Kind, PrimaryKey: true},
		{Name: "b", Type: types.Int64Kind, PrimaryKey: true},
	}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("multiple-key error = %v, want constraint", err)
	}
}

func TestNamedSnapshotLifecycleAndClone(t *testing.T) {
	schema := catalog.New()
	created, err := schema.CreateSnapshot("before-migration", 7)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 1 || created.TxnID != 7 {
		t.Fatalf("created snapshot=%+v", created)
	}
	if _, err := schema.CreateSnapshot("before-migration", 8); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate snapshot error=%v, want constraint", err)
	}
	cloned := schema.Clone()
	if err := schema.DropSnapshot("before-migration"); err != nil {
		t.Fatal(err)
	}
	if _, found := schema.LookupSnapshot("before-migration"); found {
		t.Fatal("dropped snapshot remains in source catalog")
	}
	if snapshot, found := cloned.LookupSnapshot("before-migration"); !found || snapshot.TxnID != 7 {
		t.Fatalf("clone snapshot=%+v found=%t", snapshot, found)
	}
}
