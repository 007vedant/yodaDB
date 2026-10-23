package table_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/table"
	"yodadb/types"
)

func TestPrimaryKeyTableInsertScanReplaceDeleteAndSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := table.Open(database, 0, 1, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	root1, key1, err := empty.Insert(context.Background(), database, 1, 0, types.Row{types.Int64(1), types.Text("Ada")})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := table.Open(database, root1, 1, 0, 2)
	root2, _, err := one.Insert(context.Background(), database, 2, 0, types.Row{types.Int64(2), types.Text("Grace")})
	if err != nil {
		t.Fatal(err)
	}
	two, _ := table.Open(database, root2, 1, 0, 2)
	if records, err := two.Scan(context.Background()); err != nil || len(records) != 2 || records[0].Row[1].String() != "Ada" || records[1].Row[1].String() != "Grace" {
		t.Fatalf("Scan = %#v, %v", records, err)
	}
	root3, newKey, err := two.Replace(context.Background(), database, 3, 0, key1, types.Row{types.Int64(3), types.Text("Ada Lovelace")})
	if err != nil {
		t.Fatal(err)
	}
	three, _ := table.Open(database, root3, 1, 0, 2)
	if _, found, err := three.Lookup(context.Background(), key1); err != nil || found {
		t.Fatalf("old primary key found=%t error=%v", found, err)
	}
	if row, found, err := three.Lookup(context.Background(), newKey); err != nil || !found || row[1].String() != "Ada Lovelace" {
		t.Fatalf("replacement = %#v, %t, %v", row, found, err)
	}
	if row, found, err := one.Lookup(context.Background(), key1); err != nil || !found || row[1].String() != "Ada" {
		t.Fatalf("old snapshot = %#v, %t, %v", row, found, err)
	}
	root4, deleted, err := three.Delete(context.Background(), database, 4, newKey)
	if err != nil || !deleted {
		t.Fatalf("Delete = %d, %t, %v", root4, deleted, err)
	}
	four, _ := table.Open(database, root4, 1, 0, 2)
	if records, err := four.Scan(context.Background()); err != nil || len(records) != 1 {
		t.Fatalf("post-delete Scan = %#v, %v", records, err)
	}

	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := manifest.Open(context.Background(), path, manifest.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, _ := table.Open(reopened, root4, 1, 0, 2)
	if records, err := persisted.Scan(context.Background()); err != nil || len(records) != 1 || records[0].Row[1].String() != "Grace" {
		t.Fatalf("reopened Scan = %#v, %v", records, err)
	}
}

func TestHiddenRowIDAndConstraints(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "rowid.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tree, _ := table.Open(database, 0, 1, -1, 1)
	root, key, err := tree.Insert(context.Background(), database, 7, 7, types.Row{types.Text("first")})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := table.Open(database, root, 1, -1, 1)
	if row, found, err := stored.Lookup(context.Background(), key); err != nil || !found || row[0].String() != "first" {
		t.Fatalf("Lookup = %#v, %t, %v", row, found, err)
	}
	if _, _, err := stored.Insert(context.Background(), database, 8, 0, types.Row{types.Text("bad")}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("zero RowID error = %v, want constraint", err)
	}
	if _, _, err := stored.Insert(context.Background(), database, 8, 8, types.Row{}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("wrong width error = %v, want constraint", err)
	}
}
