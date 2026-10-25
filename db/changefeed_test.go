package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/changefeed"
	"yodadb/dberrors"
)

func TestCommittedMutationsPersistInTransactionOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "logical-changes.yoda")
	database, err := CreateWithOptions(ctx, path, Options{Durability: DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	mustInternalExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)") // txn 1, no row change
	mustInternalExec(t, database, "INSERT INTO users VALUES (1, 'Ada')")                           // txn 2

	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (2, 'Grace')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET name = 'updated' WHERE id >= 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM users WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil { // txn 3, four ordered row changes
		t.Fatal(err)
	}

	assertDurableChanges(t, database, []changeExpectation{
		{txnID: 2, sequence: 0, kind: changefeed.Insert, before: "", after: "Ada"},
		{txnID: 3, sequence: 0, kind: changefeed.Insert, before: "", after: "Grace"},
		{txnID: 3, sequence: 1, kind: changefeed.Update, before: "Ada", after: "updated"},
		{txnID: 3, sequence: 2, kind: changefeed.Update, before: "Grace", after: "updated"},
		{txnID: 3, sequence: 3, kind: changefeed.Delete, before: "updated", after: ""},
	})
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = OpenWithOptions(ctx, path, Options{Durability: DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertDurableChanges(t, database, []changeExpectation{
		{txnID: 2, sequence: 0, kind: changefeed.Insert, before: "", after: "Ada"},
		{txnID: 3, sequence: 0, kind: changefeed.Insert, before: "", after: "Grace"},
		{txnID: 3, sequence: 1, kind: changefeed.Update, before: "Ada", after: "updated"},
		{txnID: 3, sequence: 2, kind: changefeed.Update, before: "Grace", after: "updated"},
		{txnID: 3, sequence: 3, kind: changefeed.Delete, before: "updated", after: ""},
	})
}

func TestRebasedChangesUseCommittedTableIDs(t *testing.T) {
	ctx := context.Background()
	database, err := CreateWithOptions(ctx, filepath.Join(t.TempDir(), "rebased-changes.yoda"), Options{Durability: DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	stale, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(ctx, "CREATE TABLE pending (id INT64 PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(ctx, "INSERT INTO pending VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	mustInternalExec(t, database, "CREATE TABLE committed (id INT64 PRIMARY KEY)")
	mustInternalExec(t, database, "INSERT INTO committed VALUES (9)")
	if err := stale.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	changes, err := changefeed.ReadAll(ctx, database.storage, database.manifest.ChangeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("change count=%d, want 2", len(changes))
	}
	committed, _ := database.catalog.LookupTable("committed")
	pending, _ := database.catalog.LookupTable("pending")
	if changes[0].TxnID != 2 || changes[0].TableID != uint64(committed.ID) || changes[1].TxnID != 3 || changes[1].TableID != uint64(pending.ID) || committed.ID == pending.ID {
		t.Fatalf("rebased change identities=%+v, committed=%d pending=%d", changes, committed.ID, pending.ID)
	}
}

func TestAbortedTransactionsPublishNoLogicalChanges(t *testing.T) {
	ctx := context.Background()
	database, err := CreateWithOptions(ctx, filepath.Join(t.TempDir(), "aborted-changes.yoda"), Options{Durability: DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustInternalExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")

	left, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.Exec(ctx, "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx, "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := left.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := right.Commit(ctx); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("conflicting commit error=%v", err)
	}

	rolledBack, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rolledBack.Exec(ctx, "INSERT INTO users VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if err := rolledBack.Rollback(); err != nil {
		t.Fatal(err)
	}
	changes, err := changefeed.ReadAll(ctx, database.storage, database.manifest.ChangeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].TxnID != 2 || changes[0].Kind != changefeed.Insert {
		t.Fatalf("durable changes=%+v, want only committed insert", changes)
	}
}

type changeExpectation struct {
	txnID    uint64
	sequence uint32
	kind     changefeed.Kind
	before   string
	after    string
}

func assertDurableChanges(t *testing.T, database *DB, want []changeExpectation) {
	t.Helper()
	changes, err := changefeed.ReadAll(context.Background(), database.storage, database.manifest.ChangeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != len(want) {
		t.Fatalf("change count=%d, want %d", len(changes), len(want))
	}
	for index, expected := range want {
		change := changes[index]
		if change.TxnID != expected.txnID || change.Sequence != expected.sequence || change.Kind != expected.kind {
			t.Fatalf("change %d metadata=%+v, want %+v", index, change, expected)
		}
		if got := imageName(change.Before); got != expected.before {
			t.Fatalf("change %d before=%q, want %q", index, got, expected.before)
		}
		if got := imageName(change.After); got != expected.after {
			t.Fatalf("change %d after=%q, want %q", index, got, expected.after)
		}
		if change.Before != nil && change.Before.SchemaVersion != 1 || change.After != nil && change.After.SchemaVersion != 1 {
			t.Fatalf("change %d schema versions before=%v after=%v", index, change.Before, change.After)
		}
	}
}

func imageName(image *changefeed.Image) string {
	if image == nil {
		return ""
	}
	value, _ := image.Row[1].TextValue()
	return value
}

func mustInternalExec(t *testing.T, database *DB, sql string) {
	t.Helper()
	if _, err := database.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}
