package db

import (
	"context"
	"errors"

	"yodadb/dberrors"
)

const defaultRetryAttempts = 3

// RetryOptions controls RetryTransaction.
type RetryOptions struct {
	// MaxAttempts includes the first execution. Zero uses the default of three.
	MaxAttempts int
}

// RetryTransaction runs fn in a fresh transaction and retries the entire
// callback after typed commit conflicts. Callers must not close the supplied
// transaction and must account for fn executing more than once.
func (db *DB) RetryTransaction(ctx context.Context, options RetryOptions, fn func(*Tx) error) error {
	if fn == nil {
		return dberrors.New(dberrors.CodeConstraint, "db.retry_transaction", "callback must not be nil")
	}
	attempts := options.MaxAttempts
	if attempts == 0 {
		attempts = defaultRetryAttempts
	}
	if attempts < 0 {
		return dberrors.New(dberrors.CodeConstraint, "db.retry_transaction", "maximum attempts must not be negative")
	}

	for attempt := 0; attempt < attempts; attempt++ {
		if err := contextError(ctx, "db.retry_transaction"); err != nil {
			return err
		}
		commitAttempted, err := db.runTransactionAttempt(ctx, fn)
		if err == nil {
			return nil
		}
		if !commitAttempted {
			return err
		}
		var conflict *ConflictError
		if !errors.As(err, &conflict) || attempt+1 == attempts {
			return err
		}
	}
	return nil
}

func (db *DB) runTransactionAttempt(ctx context.Context, fn func(*Tx) error) (commitAttempted bool, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	// Rollback is harmless after Commit closes the handle and guarantees cleanup
	// when fn returns an error or panics.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
