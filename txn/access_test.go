package txn_test

import (
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/txn"
)

func TestFootprintDeduplicatesAndSeparatesLogicalNamespaces(t *testing.T) {
	key := []byte{1, 0, 2}
	row, err := txn.NewRowResource(7, key)
	if err != nil {
		t.Fatal(err)
	}
	unique, err := txn.NewUniqueKeyResource(7, key)
	if err != nil {
		t.Fatal(err)
	}
	name, err := txn.NewCatalogNameResource(txn.CatalogTable, 0, "users")
	if err != nil {
		t.Fatal(err)
	}
	key[0] = 9

	var footprint txn.Footprint
	for _, resource := range []txn.Resource{row, row, unique, name} {
		if err := footprint.RecordRead(resource); err != nil {
			t.Fatal(err)
		}
	}
	reads := footprint.Reads()
	if len(reads) != 3 {
		t.Fatalf("read count=%d, want 3", len(reads))
	}
	if got := row.Key(); len(got) != 3 || got[0] != 1 {
		t.Fatalf("row key=%v, want defensive original", got)
	}
	got := row.Key()
	got[0] = 8
	if row.Key()[0] != 1 {
		t.Fatal("Resource.Key did not return a defensive copy")
	}
}

func TestFootprintWriteConflictsUseOnlyExactWrites(t *testing.T) {
	row1, _ := txn.NewRowResource(1, []byte("row-1"))
	row2, _ := txn.NewRowResource(1, []byte("row-2"))
	unique, _ := txn.NewUniqueKeyResource(4, []byte("ada@example.com"))
	catalogName, _ := txn.NewCatalogNameResource(txn.CatalogTable, 0, "users")

	var left, right txn.Footprint
	for _, resource := range []txn.Resource{row1, unique, catalogName} {
		if err := left.RecordWrite(resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := left.RecordRead(row2); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []txn.Resource{row1, row2, catalogName} {
		if err := right.RecordWrite(resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := right.RecordRead(unique); err != nil {
		t.Fatal(err)
	}

	conflicts := left.WriteConflicts(&right)
	if len(conflicts) != 2 || conflicts[0] != row1 || conflicts[1] != catalogName {
		t.Fatalf("conflicts=%v, want row1 and catalog name", conflicts)
	}
	if got := (&txn.Footprint{}).WriteConflicts(&right); len(got) != 0 {
		t.Fatalf("empty footprint conflicts=%v", got)
	}
}

func TestCatalogObjectAndNameAreDistinctConflictIdentities(t *testing.T) {
	object, err := txn.NewCatalogObjectResource(txn.CatalogIndex, 12)
	if err != nil {
		t.Fatal(err)
	}
	name, err := txn.NewCatalogNameResource(txn.CatalogIndex, 7, "users_email")
	if err != nil {
		t.Fatal(err)
	}
	if object == name {
		t.Fatal("catalog object ID and name resources must be distinct")
	}
	if object.ScopeID() != 12 || object.CatalogKind() != txn.CatalogIndex || len(object.Key()) != 0 {
		t.Fatalf("unexpected object resource: scope=%d catalog=%d key=%v", object.ScopeID(), object.CatalogKind(), object.Key())
	}
	if name.ScopeID() != 7 || string(name.Key()) != "users_email" {
		t.Fatalf("unexpected name resource: scope=%d key=%q", name.ScopeID(), name.Key())
	}
}

func TestResourceConstructorsRejectAmbiguousIdentities(t *testing.T) {
	tests := []struct {
		name string
		make func() error
	}{
		{"zero table", func() error { _, err := txn.NewRowResource(0, []byte{1}); return err }},
		{"empty row key", func() error { _, err := txn.NewRowResource(1, nil); return err }},
		{"zero index", func() error { _, err := txn.NewUniqueKeyResource(0, []byte{1}); return err }},
		{"zero object", func() error { _, err := txn.NewCatalogObjectResource(txn.CatalogTable, 0); return err }},
		{"empty name", func() error { _, err := txn.NewCatalogNameResource(txn.CatalogTable, 0, ""); return err }},
		{"index without owner", func() error { _, err := txn.NewCatalogNameResource(txn.CatalogIndex, 0, "by_email"); return err }},
		{"global name with owner", func() error { _, err := txn.NewCatalogNameResource(txn.CatalogTable, 7, "users"); return err }},
		{"invalid catalog kind", func() error { _, err := txn.NewCatalogObjectResource(99, 1); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.make(); !errors.Is(err, dberrors.ErrConstraint) {
				t.Fatalf("error=%v, want constraint", err)
			}
		})
	}

	var footprint txn.Footprint
	if err := footprint.RecordWrite(txn.Resource{}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("RecordWrite zero resource error=%v, want constraint", err)
	}
}

func TestIndexNamesAreScopedByOwningTable(t *testing.T) {
	left, err := txn.NewCatalogNameResource(txn.CatalogIndex, 1, "by_email")
	if err != nil {
		t.Fatal(err)
	}
	right, err := txn.NewCatalogNameResource(txn.CatalogIndex, 2, "by_email")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("same index name on different tables must not share an identity")
	}
}
