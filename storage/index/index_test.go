package index_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/index"
	"yodadb/storage/manifest"
	"yodadb/types"
)

func TestUniqueAndNonUniqueIndexMaintenance(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "indexes.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	unique, _ := index.Open(database, 0, index.Definition{Columns: []int{1}, Unique: true})
	row1 := types.Row{types.Int64(1), types.Text("ada@example.test")}
	root, err := unique.Insert(context.Background(), database, 1, row1, []byte("pk1"))
	if err != nil {
		t.Fatal(err)
	}
	unique1, _ := index.Open(database, root, index.Definition{Columns: []int{1}, Unique: true})
	if _, err := unique1.Insert(context.Background(), database, 2, types.Row{types.Int64(2), types.Text("ada@example.test")}, []byte("pk2")); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate unique key error = %v, want constraint", err)
	}
	keys, err := unique1.Lookup(context.Background(), types.Row{types.Text("ada@example.test")})
	if err != nil || len(keys) != 1 || string(keys[0]) != "pk1" {
		t.Fatalf("unique Lookup = %q, %v", keys, err)
	}

	nonUnique, _ := index.Open(database, 0, index.Definition{Columns: []int{1}})
	root, err = nonUnique.Insert(context.Background(), database, 3, row1, []byte("pk1"))
	if err != nil {
		t.Fatal(err)
	}
	nonUnique, _ = index.Open(database, root, index.Definition{Columns: []int{1}})
	root, err = nonUnique.Insert(context.Background(), database, 4, types.Row{types.Int64(2), types.Text("ada@example.test")}, []byte("pk2"))
	if err != nil {
		t.Fatal(err)
	}
	nonUnique, _ = index.Open(database, root, index.Definition{Columns: []int{1}})
	keys, err = nonUnique.Lookup(context.Background(), types.Row{types.Text("ada@example.test")})
	if err != nil || len(keys) != 2 || string(keys[0]) != "pk1" || string(keys[1]) != "pk2" {
		t.Fatalf("non-unique Lookup = %q, %v", keys, err)
	}
	deletedRoot, deleted, err := nonUnique.Delete(context.Background(), database, 5, row1, []byte("pk1"))
	if err != nil || !deleted {
		t.Fatalf("Delete = %d, %t, %v", deletedRoot, deleted, err)
	}
	afterDelete, _ := index.Open(database, deletedRoot, index.Definition{Columns: []int{1}})
	keys, err = afterDelete.Lookup(context.Background(), types.Row{types.Text("ada@example.test")})
	if err != nil || len(keys) != 1 || string(keys[0]) != "pk2" {
		t.Fatalf("post-delete Lookup = %q, %v", keys, err)
	}
	oldKeys, _ := nonUnique.Lookup(context.Background(), types.Row{types.Text("ada@example.test")})
	if len(oldKeys) != 2 {
		t.Fatalf("old index snapshot has %d keys, want 2", len(oldKeys))
	}
}

func TestUniqueIndexAllowsMultipleNulls(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "nulls.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tree, _ := index.Open(database, 0, index.Definition{Columns: []int{0}, Unique: true})
	root, err := tree.Insert(context.Background(), database, 1, types.Row{types.Null()}, []byte("pk1"))
	if err != nil {
		t.Fatal(err)
	}
	tree, _ = index.Open(database, root, index.Definition{Columns: []int{0}, Unique: true})
	root, err = tree.Insert(context.Background(), database, 2, types.Row{types.Null()}, []byte("pk2"))
	if err != nil {
		t.Fatal(err)
	}
	tree, _ = index.Open(database, root, index.Definition{Columns: []int{0}, Unique: true})
	keys, err := tree.Lookup(context.Background(), types.Row{types.Null()})
	if err != nil || len(keys) != 2 {
		t.Fatalf("NULL Lookup = %q, %v", keys, err)
	}
}

func TestEncodeUniqueKeyTracksOnlyEnforcedKeys(t *testing.T) {
	row := types.Row{types.Int64(7), types.Text("ada@example.test")}
	key, applicable, err := index.EncodeUniqueKey(index.Definition{Columns: []int{1}, Unique: true}, row)
	if err != nil || !applicable {
		t.Fatalf("EncodeUniqueKey applicable=%t error=%v", applicable, err)
	}
	want, err := types.EncodeKey(types.Row{types.Text("ada@example.test")}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != string(want) {
		t.Fatalf("unique key=%x, want %x", key, want)
	}
	if _, applicable, err := index.EncodeUniqueKey(index.Definition{Columns: []int{1}}, row); err != nil || applicable {
		t.Fatalf("non-unique applicable=%t error=%v", applicable, err)
	}
	if _, applicable, err := index.EncodeUniqueKey(index.Definition{Columns: []int{1}, Unique: true}, types.Row{types.Int64(7), types.Null()}); err != nil || applicable {
		t.Fatalf("NULL unique applicable=%t error=%v", applicable, err)
	}
}
