package catalog_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
	"yodadb/types"
)

func TestPersistentCatalogRoundTripAndStableIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	schema := catalog.New()
	users, err := schema.CreateTable("users", []catalog.ColumnSpec{
		{Name: "id", Type: types.Int64Kind, PrimaryKey: true},
		{Name: "email", Type: types.TextKind, NotNull: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	emails, err := schema.CreateIndex(users.ID, "users_email", []catalog.ColumnID{users.Columns[1].ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	root1, err := catalog.Save(context.Background(), database, 1, schema)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(context.Background(), database, root1)
	if err != nil {
		t.Fatal(err)
	}
	gotUsers, ok := loaded.LookupTable("users")
	if !ok || gotUsers.ID != users.ID || gotUsers.Columns[1].ID != users.Columns[1].ID {
		t.Fatalf("loaded users = %#v, found=%t", gotUsers, ok)
	}
	gotEmails, ok := loaded.LookupIndex(users.ID, "users_email")
	if !ok || gotEmails.ID != emails.ID || !gotEmails.Unique || len(gotEmails.Columns) != 1 || gotEmails.Columns[0] != users.Columns[1].ID {
		t.Fatalf("loaded index = %#v, found=%t", gotEmails, ok)
	}
	posts, err := loaded.CreateTable("posts", []catalog.ColumnSpec{{Name: "body", Type: types.TextKind}})
	if err != nil {
		t.Fatal(err)
	}
	if posts.ID != users.ID+1 || posts.Columns[0].ID != users.Columns[1].ID+1 {
		t.Fatalf("IDs after reload: table=%d column=%d", posts.ID, posts.Columns[0].ID)
	}
	root2, err := catalog.Save(context.Background(), database, 2, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if root2 == root1 {
		t.Fatal("catalog mutation reused immutable root")
	}
	oldSnapshot, err := catalog.Load(context.Background(), database, root1)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := oldSnapshot.LookupTable("posts"); found {
		t.Fatal("old catalog root observes later table")
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
	persisted, err := catalog.Load(context.Background(), reopened, root2)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := persisted.LookupTable("posts"); !found {
		t.Fatal("reopened catalog is missing posts")
	}
}

func TestPersistentCatalogRejectsInvalidInputsAndPageType(t *testing.T) {
	if _, err := catalog.Save(context.Background(), nil, 1, catalog.New()); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("nil store error = %v, want constraint", err)
	}
	source := catalogPageSource{2: {Header: page.Header{PageID: 2, Type: page.TypeCatalog}}}
	if _, err := catalog.Load(context.Background(), source, 2); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("wrong root type error = %v, want corruption", err)
	}
}

func TestPersistentCatalogRoundTripsNamedSnapshots(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "snapshot-catalog.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	schema := catalog.New()
	created, err := schema.CreateSnapshot("release-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.Save(context.Background(), database, 1, schema)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(context.Background(), database, root)
	if err != nil {
		t.Fatal(err)
	}
	got, found := loaded.LookupSnapshot("release-1")
	if !found || got != created {
		t.Fatalf("loaded snapshot=%+v found=%t, want %+v", got, found, created)
	}
	if err := loaded.DropSnapshot("release-1"); err != nil {
		t.Fatal(err)
	}
	if snapshots := loaded.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("snapshots after drop=%+v", snapshots)
	}
}

type catalogPageSource map[page.ID]page.Page

func (s catalogPageSource) ReadPage(id page.ID) (page.Page, error) {
	stored, ok := s[id]
	if !ok {
		return page.Page{}, errors.New("missing page")
	}
	return stored, nil
}
