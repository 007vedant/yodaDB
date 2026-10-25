package db_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

func TestRetryTransactionRerunsWholeCallbackAfterCommitConflict(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "retry-success.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'original')")

	attempts := 0
	err = database.RetryTransaction(ctx, db.RetryOptions{MaxAttempts: 2}, func(tx *db.Tx) error {
		attempts++
		if _, err := tx.Exec(ctx, "UPDATE users SET name = 'retried' WHERE id = 1"); err != nil {
			return err
		}
		if attempts != 1 {
			return nil
		}
		competitor, err := database.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := competitor.Exec(ctx, "UPDATE users SET name = 'competitor' WHERE id = 1"); err != nil {
			_ = competitor.Rollback()
			return err
		}
		return competitor.Commit(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("callback attempts=%d, want 2", attempts)
	}
	assertStrings(t, database, "SELECT name FROM users WHERE id = 1", []string{"retried"})
}

func TestRetryTransactionStopsAtAttemptLimit(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "retry-limit.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'original')")

	attempts := 0
	err = database.RetryTransaction(ctx, db.RetryOptions{MaxAttempts: 2}, func(tx *db.Tx) error {
		attempts++
		if _, err := tx.Exec(ctx, "UPDATE users SET name = 'candidate' WHERE id = 1"); err != nil {
			return err
		}
		competitor, err := database.Begin(ctx)
		if err != nil {
			return err
		}
		query := fmt.Sprintf("UPDATE users SET name = 'competitor-%d' WHERE id = 1", attempts)
		if _, err := competitor.Exec(ctx, query); err != nil {
			_ = competitor.Rollback()
			return err
		}
		return competitor.Commit(ctx)
	})
	var conflict *db.ConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("RetryTransaction error=%v, want typed conflict", err)
	}
	if attempts != 2 {
		t.Fatalf("callback attempts=%d, want 2", attempts)
	}
	assertStrings(t, database, "SELECT name FROM users WHERE id = 1", []string{"competitor-2"})
}

func TestRetryTransactionDoesNotRetryCallbackErrors(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "retry-callback-error.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")

	want := errors.New("callback failed")
	attempts := 0
	err = database.RetryTransaction(ctx, db.RetryOptions{MaxAttempts: 3}, func(tx *db.Tx) error {
		attempts++
		if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (1)"); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("RetryTransaction error=%v, want callback error", err)
	}
	if attempts != 1 {
		t.Fatalf("callback attempts=%d, want 1", attempts)
	}
	assertNoRow(t, database, "SELECT id FROM users")

	attempts = 0
	callbackConflict := &db.ConflictError{Kind: db.ConflictWriteWrite}
	err = database.RetryTransaction(ctx, db.RetryOptions{MaxAttempts: 3}, func(*db.Tx) error {
		attempts++
		return callbackConflict
	})
	if err != callbackConflict || attempts != 1 {
		t.Fatalf("typed callback conflict error=%v attempts=%d, want original error after one attempt", err, attempts)
	}
}

func TestRetryTransactionValidatesOptions(t *testing.T) {
	database := db.New()
	if err := database.RetryTransaction(context.Background(), db.RetryOptions{}, nil); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("nil callback error=%v", err)
	}
	if err := database.RetryTransaction(context.Background(), db.RetryOptions{MaxAttempts: -1}, func(*db.Tx) error { return nil }); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("negative attempts error=%v", err)
	}
}
