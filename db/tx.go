package db

import (
	"context"

	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/parser"
	"yodadb/txn"
)

// Tx is the native handle for the single active write transaction. Reads use
// its private roots, so statements observe earlier writes before commit.
type Tx struct {
	db *DB
	id uint64
}

// Begin starts the database's only active write transaction.
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "db.begin", "database is closed")
	}
	if err := contextError(ctx, "db.begin"); err != nil {
		return nil, err
	}
	if db.storage == nil {
		return nil, dberrors.New(dberrors.CodeUnsupported, "db.begin", "explicit transactions require a persistent database")
	}
	if db.nextTxID == ^uint64(0) {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "db.begin", "transaction handle ID space is exhausted")
	}
	db.nextTxID++
	owner := db.nextTxID
	if err := db.beginWriteLocked(owner); err != nil {
		return nil, err
	}
	return &Tx{db: db, id: owner}, nil
}

// Execute parses, binds, and executes a statement in the transaction.
func (tx *Tx) Execute(ctx context.Context, sql string, arguments ...any) (Outcome, error) {
	if tx == nil || tx.db == nil {
		return Outcome{}, txClosed("tx.execute")
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	return tx.db.executeSQLLocked(ctx, tx.id, sql, arguments)
}

// Exec executes a non-query statement in the transaction.
func (tx *Tx) Exec(ctx context.Context, sql string, arguments ...any) (Result, error) {
	if tx == nil || tx.db == nil {
		return Result{}, txClosed("tx.exec")
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	return tx.db.execSQLLocked(ctx, tx.id, sql, arguments)
}

// Query executes a SELECT or EXPLAIN against the transaction's private roots.
func (tx *Tx) Query(ctx context.Context, sql string, arguments ...any) (*Rows, error) {
	if tx == nil || tx.db == nil {
		return nil, txClosed("tx.query")
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	return tx.db.querySQLLocked(ctx, tx.id, sql, arguments)
}

// Commit publishes all successful statements as one manifest transaction.
func (tx *Tx) Commit(ctx context.Context) error {
	if tx == nil || tx.db == nil {
		return txClosed("tx.commit")
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	return tx.db.commitWriteLocked(ctx, tx.id)
}

// Rollback discards the transaction's private roots. Appended COW pages remain
// unreachable and can be reclaimed by future compaction.
func (tx *Tx) Rollback() error {
	if tx == nil || tx.db == nil {
		return txClosed("tx.rollback")
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	return tx.db.rollbackWriteLocked(tx.id)
}

func (db *DB) executeTransactionControl(ctx context.Context, owner uint64, control *binder.TransactionControl) (Outcome, error) {
	if owner != 0 {
		return Outcome{}, dberrors.New(dberrors.CodeUnsupported, "db.transaction_control", "use Tx.Commit or Tx.Rollback on a transaction handle")
	}
	switch control.Action {
	case parser.BeginTransaction:
		if err := contextError(ctx, "db.begin"); err != nil {
			return Outcome{}, err
		}
		if db.storage == nil {
			return Outcome{}, dberrors.New(dberrors.CodeUnsupported, "db.begin", "explicit transactions require a persistent database")
		}
		return Outcome{}, db.beginWriteLocked(0)
	case parser.CommitTransaction:
		return Outcome{}, db.commitWriteLocked(ctx, 0)
	case parser.RollbackTransaction:
		return Outcome{}, db.rollbackWriteLocked(0)
	default:
		return Outcome{}, dberrors.New(dberrors.CodeUnsupported, "db.transaction_control", "unknown transaction action")
	}
}

func (db *DB) beginWriteLocked(owner uint64) error {
	if db.writeTxn != nil {
		return dberrors.New(dberrors.CodeConflict, "db.begin", "a write transaction is already active")
	}
	write, err := txn.BeginWrite(db.manifest)
	if err != nil {
		return err
	}
	db.writeTxn = write
	db.txOwner = owner
	return nil
}

func (db *DB) commitWriteLocked(ctx context.Context, owner uint64) error {
	if err := db.requireOwner(owner); err != nil {
		return err
	}
	if err := contextError(ctx, "db.commit"); err != nil {
		_ = db.writeTxn.Rollback()
		db.clearWrite()
		return err
	}
	record, dirty, err := db.writeTxn.PrepareCommit()
	if err != nil {
		return err
	}
	if dirty {
		if err := db.publish(ctx, record); err != nil {
			db.writeTxn.CommitFailed()
			db.clearWrite()
			return err
		}
	}
	if err := db.writeTxn.CommitSucceeded(); err != nil {
		db.clearWrite()
		return err
	}
	db.clearWrite()
	return nil
}

func (db *DB) rollbackWriteLocked(owner uint64) error {
	if err := db.requireOwner(owner); err != nil {
		return err
	}
	err := db.writeTxn.Rollback()
	db.clearWrite()
	return err
}

func (db *DB) checkOwner(owner uint64) error {
	if owner == 0 {
		if db.writeTxn != nil && db.txOwner != 0 {
			return dberrors.New(dberrors.CodeConflict, "db.transaction", "database has an active transaction handle")
		}
		return nil
	}
	return db.requireOwner(owner)
}

func (db *DB) requireOwner(owner uint64) error {
	if db.writeTxn == nil || db.txOwner != owner {
		return txClosed("db.transaction")
	}
	return nil
}

func (db *DB) clearWrite() {
	db.writeTxn = nil
	db.txOwner = 0
}

func contextError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return dberrors.Wrap(err, dberrors.CodeCancelled, operation, "context cancelled")
	}
	return nil
}

func txClosed(operation string) error {
	return dberrors.New(dberrors.CodeTransactionClosed, operation, "transaction is not active")
}

func transactionalDDLError() error {
	return dberrors.New(dberrors.CodeUnsupported, "db.ddl", "transactional DDL is deferred until M5.7")
}
