package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
	"yodadb/storage/manifest"
)

func TestTxReadOwnWritesAndOneManifestCommit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "transaction.yoda")
	database, err := db.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE events (value TEXT NOT NULL)")

	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO events VALUES ('alpha')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO events VALUES ('beta')"); err != nil {
		t.Fatal(err)
	}
	assertTxStrings(t, tx, "SELECT value FROM events", []string{"alpha", "beta"})
	assertNoRow(t, database, "SELECT value FROM events")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO events VALUES ('late')"); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("Exec after commit error=%v, want transaction closed", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.Open(ctx, path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := store.LatestManifest()
	if err != nil || !found {
		t.Fatalf("latest manifest found=%t error=%v", found, err)
	}
	if record.TxnID != 2 {
		t.Fatalf("transaction ID=%d, want 2 (CREATE plus one multi-statement commit)", record.TxnID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertStrings(t, database, "SELECT value FROM events", []string{"alpha", "beta"})
}

func TestConcurrentTransactionsPreparePrivateWrites(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "concurrent-preparation.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")

	left, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByWriter := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	prepare := func(tx *db.Tx, sql string) {
		ready.Done()
		<-start
		_, err := tx.Exec(ctx, sql)
		errorsByWriter <- err
	}
	go prepare(left, "INSERT INTO users VALUES (1, 'Ada')")
	go prepare(right, "INSERT INTO users VALUES (2, 'Grace')")
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errorsByWriter; err != nil {
			t.Fatal(err)
		}
	}

	assertTxStrings(t, left, "SELECT name FROM users WHERE id = 1", []string{"Ada"})
	assertTxStrings(t, right, "SELECT name FROM users WHERE id = 2", []string{"Grace"})
	assertNoRow(t, database, "SELECT name FROM users")
	if err := left.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := right.Commit(ctx); err != nil {
		t.Fatalf("disjoint stale commit after rebase: %v", err)
	}
	assertStrings(t, database, "SELECT name FROM users", []string{"Ada", "Grace"})
}

func TestCommitCoordinatorRejectsSameRowWrite(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "same-row-conflict.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'original')")

	left, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.Exec(ctx, "UPDATE users SET name = 'left' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx, "UPDATE users SET name = 'right' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := left.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := right.Commit(ctx); !errors.Is(err, dberrors.ErrConflict) || !strings.Contains(err.Error(), "logical write conflict") {
		t.Fatalf("same-row commit error=%v, want logical write conflict", err)
	}
	assertStrings(t, database, "SELECT name FROM users WHERE id = 1", []string{"left"})
}

func TestCommitCoordinatorChecksAllTransactionsSinceBase(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "conflict-history.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'one')")
	mustExec(t, database, "INSERT INTO users VALUES (2, 'two')")

	stale, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(ctx, "UPDATE users SET name = 'stale' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "UPDATE users SET name = 'committed' WHERE id = 1")
	mustExec(t, database, "UPDATE users SET name = 'newer' WHERE id = 2")
	if err := stale.Commit(ctx); !errors.Is(err, dberrors.ErrConflict) || !strings.Contains(err.Error(), "logical write conflict") {
		t.Fatalf("historical conflict error=%v", err)
	}
	assertStrings(t, database, "SELECT name FROM users WHERE id = 1", []string{"committed"})
}

func TestPrivateWriterDoesNotBlockCommittedAutocommit(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "writer-and-autocommit.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")

	private, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := private.Exec(ctx, "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "INSERT INTO users VALUES (2)")
	assertOneInt64(t, database, "SELECT id FROM users WHERE id = 2", 2)
	if err := private.Commit(ctx); err != nil {
		t.Fatalf("stale private commit after rebase: %v", err)
	}
	assertOneInt64(t, database, "SELECT id FROM users WHERE id = 1", 1)
}

func TestRebaseReplaysExactRowsWithoutReevaluatingPredicate(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "exact-row-replay.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, state TEXT NOT NULL, note TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'ready', 'original')")

	stale, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(ctx, "UPDATE users SET note = 'prepared' WHERE state = 'ready'"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "INSERT INTO users VALUES (2, 'ready', 'newer')")
	if err := stale.Commit(ctx); err != nil {
		t.Fatalf("exact-row rebase: %v", err)
	}
	assertStrings(t, database, "SELECT note FROM users WHERE id = 1", []string{"prepared"})
	assertStrings(t, database, "SELECT note FROM users WHERE id = 2", []string{"newer"})
}

func TestRebaseMaintainsSecondaryIndexesForDisjointUpdates(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "rebase-indexes.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT NOT NULL)")
	mustExec(t, database, "CREATE UNIQUE INDEX users_email ON users (email)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'one@example.com')")
	mustExec(t, database, "INSERT INTO users VALUES (2, 'two@example.com')")

	left, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.Exec(ctx, "UPDATE users SET email = 'left@example.com' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx, "UPDATE users SET email = 'right@example.com' WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if err := left.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := right.Commit(ctx); err != nil {
		t.Fatalf("indexed rebase: %v", err)
	}
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'left@example.com'", 1)
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'right@example.com'", 2)
}

func TestRebaseReplaysDeleteBesideNewerInsert(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "rebase-delete.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	mustExec(t, database, "INSERT INTO users VALUES (1)")

	stale, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(ctx, "DELETE FROM users WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "INSERT INTO users VALUES (2)")
	if err := stale.Commit(ctx); err != nil {
		t.Fatalf("delete rebase: %v", err)
	}
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 1")
	assertOneInt64(t, database, "SELECT id FROM users WHERE id = 2", 2)
}

func TestSQLTransactionRebasesAfterNativeCommit(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "sql-rebase.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'one')")
	mustExec(t, database, "INSERT INTO users VALUES (2, 'two')")

	mustExec(t, database, "BEGIN")
	mustExec(t, database, "UPDATE users SET name = 'sql' WHERE id = 1")
	native, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Exec(ctx, "UPDATE users SET name = 'native' WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if err := native.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "COMMIT")
	assertStrings(t, database, "SELECT name FROM users WHERE id = 1", []string{"sql"})
	assertStrings(t, database, "SELECT name FROM users WHERE id = 2", []string{"native"})
}

func TestStaleCatalogMutationWaitsForCatalogValidation(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "stale-ddl.yoda"))
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
	mustExec(t, database, "CREATE TABLE committed (id INT64 PRIMARY KEY)")
	if err := stale.Commit(ctx); !errors.Is(err, dberrors.ErrConflict) || !strings.Contains(err.Error(), "M7.5") {
		t.Fatalf("stale DDL commit error=%v, want deferred catalog conflict", err)
	}
	if _, err := database.Query(ctx, "SELECT id FROM pending"); !errors.Is(err, dberrors.ErrBind) {
		t.Fatalf("pending table query error=%v, want bind error", err)
	}
}

func TestReadOnlyPrivateTransactionCanCloseAfterSnapshotBecomesStale(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "stale-read-only-writer.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")

	readOnly, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertNoTxRow(t, readOnly, "SELECT id FROM users")
	mustExec(t, database, "INSERT INTO users VALUES (1)")
	assertNoTxRow(t, readOnly, "SELECT id FROM users")
	if err := readOnly.Commit(ctx); err != nil {
		t.Fatalf("read-only stale commit error=%v", err)
	}
}

func TestPrivateWriterPinsItsBaseAgainstHistoryRetention(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "writer-history-pin.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")

	writer, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "INSERT INTO users VALUES (1)")
	if err := database.RetainHistoryFrom(ctx, 2); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("retention past writer base error=%v, want conflict", err)
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := database.RetainHistoryFrom(ctx, 2); err != nil {
		t.Fatalf("retention after writer rollback: %v", err)
	}
}

func TestCloseAbortsAllPrivateWriters(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "close-writers.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	first, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Rollback(); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("first rollback after close error=%v", err)
	}
	if _, err := second.Query(ctx, "SELECT id FROM users"); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("second query after close error=%v", err)
	}
}

func TestTxRollbackLeavesNoReachableWrites(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "rollback.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	assertTxStrings(t, tx, "SELECT name FROM users WHERE id = 1", []string{"Ada"})
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertNoRow(t, database, "SELECT name FROM users WHERE id = 1")
	if err := tx.Rollback(); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("second rollback error=%v", err)
	}
}

func TestTxTransactionalDDLCommitsCatalogAndDataTogether(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "transactional-ddl.yoda")
	database, err := db.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (1, 'ada@example.com')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "CREATE UNIQUE INDEX users_email ON users (email)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (2, 'grace@example.com')"); err != nil {
		t.Fatal(err)
	}
	assertTxStrings(t, tx, "SELECT email FROM users WHERE id = 1", []string{"ada@example.com"})
	assertTxStrings(t, tx, "EXPLAIN SELECT id FROM users WHERE email = 'grace@example.com'", []string{"INDEX LOOKUP users_email ON users(email)"})
	if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (3, 'ada@example.com')"); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate error=%v, want constraint", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.Open(ctx, path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := store.LatestManifest()
	if err != nil || !found {
		t.Fatalf("latest manifest found=%t error=%v", found, err)
	}
	if record.TxnID != 1 {
		t.Fatalf("transaction ID=%d, want 1 for one atomic DDL/DML commit", record.TxnID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertStrings(t, database, "SELECT email FROM users WHERE id >= 1", []string{"ada@example.com", "grace@example.com"})
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE email = 'ada@example.com'", "INDEX LOOKUP users_email ON users(email)")
}

func TestSQLTransactionCommitRollbackAndErrors(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "sql-transaction.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT NOT NULL)")
	mustExec(t, database, "CREATE UNIQUE INDEX users_email ON users (email)")

	mustExec(t, database, "BEGIN")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'ada@example.com')")
	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (2, 'ada@example.com')"); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate error=%v, want constraint", err)
	}
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'ada@example.com'", 1)
	mustExec(t, database, "CREATE TABLE deferred (id INT64)")
	mustExec(t, database, "INSERT INTO deferred VALUES (10)")
	assertOneInt64(t, database, "SELECT id FROM deferred", 10)
	mustExec(t, database, "ROLLBACK")
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 1")
	if _, err := database.Query(ctx, "SELECT id FROM deferred"); !errors.Is(err, dberrors.ErrBind) {
		t.Fatalf("rolled-back table query error=%v, want bind error", err)
	}

	mustExec(t, database, "BEGIN")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'ada@example.com')")
	if _, err := database.Exec(ctx, "BEGIN"); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("nested BEGIN error=%v", err)
	}
	mustExec(t, database, "COMMIT")
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'ada@example.com'", 1)
	if _, err := database.Exec(ctx, "COMMIT"); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("COMMIT without transaction error=%v", err)
	}
}

func TestTxCancelledCommitAbortsPrivateState(t *testing.T) {
	database, err := db.Create(context.Background(), filepath.Join(t.TempDir(), "cancel.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	tx, err := database.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "CREATE TABLE pending (id INT64 PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "INSERT INTO pending VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tx.Commit(cancelled); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("Commit error=%v, want cancelled", err)
	}
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 1")
	if _, err := database.Query(context.Background(), "SELECT id FROM pending"); !errors.Is(err, dberrors.ErrBind) {
		t.Fatalf("cancelled DDL query error=%v, want bind error", err)
	}
}

func assertTxStrings(t *testing.T, tx *db.Tx, query string, want []string) {
	t.Helper()
	rows, err := tx.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if len(got) != len(want) {
		t.Fatalf("query %q returned %v, want %v", query, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("query %q returned %v, want %v", query, got, want)
		}
	}
}

func assertNoTxRow(t *testing.T, tx *db.Tx, query string) {
	t.Helper()
	rows, err := tx.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatalf("query %q unexpectedly returned a row", query)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func assertStrings(t *testing.T, database *db.DB, query string, want []string) {
	t.Helper()
	rows, err := database.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if len(got) != len(want) {
		t.Fatalf("query %q returned %v, want %v", query, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("query %q returned %v, want %v", query, got, want)
		}
	}
}
