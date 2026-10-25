package db

import (
	"context"
	"fmt"
	"sync"

	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/parser"
	"yodadb/txn"
)

// Tx is a native write transaction with a private catalog and root set. Each
// handle serializes its own statements, while separate handles may prepare
// changes concurrently from committed snapshots.
type Tx struct {
	mu        sync.Mutex
	db        *DB
	work      *DB
	id        uint64
	baseTxnID uint64
}

// Begin starts a write transaction at the latest committed snapshot.
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	db.writerLifecycle.RLock()
	defer db.writerLifecycle.RUnlock()
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
	base := cloneManifest(db.manifest)
	write, err := txn.BeginWrite(base)
	if err != nil {
		return nil, err
	}
	privateCatalog := db.catalog.Clone()
	work := &DB{
		catalog:      privateCatalog,
		storage:      db.storage,
		manifest:     base,
		writeTxn:     write,
		writeCatalog: privateCatalog,
		txOwner:      owner,
	}
	handle := &Tx{db: db, work: work, id: owner, baseTxnID: base.TxnID}
	if db.writers == nil {
		db.writers = make(map[uint64]*Tx)
	}
	db.writers[owner] = handle
	db.pinSnapshot(base.TxnID)
	return handle, nil
}

// Execute parses, binds, and executes a statement in the transaction.
func (tx *Tx) Execute(ctx context.Context, sql string, arguments ...any) (Outcome, error) {
	if tx == nil || tx.db == nil {
		return Outcome{}, txClosed("tx.execute")
	}
	tx.db.writerLifecycle.RLock()
	defer tx.db.writerLifecycle.RUnlock()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.db.closed || tx.work == nil {
		return Outcome{}, txClosed("tx.execute")
	}
	return tx.work.executeSQLLocked(ctx, tx.id, sql, arguments)
}

// Exec executes a non-query statement in the transaction.
func (tx *Tx) Exec(ctx context.Context, sql string, arguments ...any) (Result, error) {
	if tx == nil || tx.db == nil {
		return Result{}, txClosed("tx.exec")
	}
	tx.db.writerLifecycle.RLock()
	defer tx.db.writerLifecycle.RUnlock()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.db.closed || tx.work == nil {
		return Result{}, txClosed("tx.exec")
	}
	return tx.work.execSQLLocked(ctx, tx.id, sql, arguments)
}

// Query executes a SELECT or EXPLAIN against the transaction's private roots.
func (tx *Tx) Query(ctx context.Context, sql string, arguments ...any) (*Rows, error) {
	if tx == nil || tx.db == nil {
		return nil, txClosed("tx.query")
	}
	tx.db.writerLifecycle.RLock()
	defer tx.db.writerLifecycle.RUnlock()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.db.closed || tx.work == nil {
		return nil, txClosed("tx.query")
	}
	return tx.work.querySQLLocked(ctx, tx.id, sql, arguments)
}

// Commit publishes all successful statements as one manifest transaction.
func (tx *Tx) Commit(ctx context.Context) error {
	if tx == nil || tx.db == nil {
		return txClosed("tx.commit")
	}
	tx.db.writerLifecycle.RLock()
	defer tx.db.writerLifecycle.RUnlock()
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.db.closed || tx.work == nil || tx.db.writers[tx.id] != tx {
		return txClosed("tx.commit")
	}
	if err := contextError(ctx, "tx.commit"); err != nil {
		_ = tx.work.writeTxn.Rollback()
		tx.db.closeNativeWriteLocked(tx)
		return err
	}
	record, dirty, err := tx.work.writeTxn.PrepareCommit()
	if err != nil {
		return err
	}
	rebased := false
	rebasedCatalog := tx.db.catalog
	if dirty {
		validation, err := tx.db.commitCoordinator().Validate(record.ParentTxnID, &tx.work.writeFootprint)
		if err != nil {
			tx.work.writeTxn.CommitFailed()
			tx.db.closeNativeWriteLocked(tx)
			return err
		}
		if len(validation.Conflicts) != 0 {
			tx.work.writeTxn.CommitFailed()
			tx.db.closeNativeWriteLocked(tx)
			return dberrors.New(dberrors.CodeConflict, "tx.commit", fmt.Sprintf("logical write conflict on %d resource(s) since transaction %d", len(validation.Conflicts), record.ParentTxnID))
		}
		if validation.LatestTxnID != record.ParentTxnID {
			record, rebasedCatalog, err = tx.db.replayTransaction(ctx, tx.work.catalogMutations, tx.work.writeMutations)
			if err != nil {
				tx.work.writeTxn.CommitFailed()
				tx.db.closeNativeWriteLocked(tx)
				return err
			}
			rebased = true
		}
	}
	if dirty {
		if err := tx.db.publishWithFootprint(ctx, record, &tx.work.writeFootprint); err != nil {
			tx.work.writeTxn.CommitFailed()
			tx.db.closeNativeWriteLocked(tx)
			return err
		}
	}
	if err := tx.work.writeTxn.CommitSucceeded(); err != nil {
		tx.db.closeNativeWriteLocked(tx)
		return err
	}
	if dirty {
		if rebased {
			tx.db.catalog = rebasedCatalog
		} else {
			tx.db.catalog = tx.work.writeCatalog
		}
	}
	tx.db.closeNativeWriteLocked(tx)
	return nil
}

// Rollback discards the transaction's private roots. Appended COW pages remain
// unreachable and can be reclaimed by future compaction.
func (tx *Tx) Rollback() error {
	if tx == nil || tx.db == nil {
		return txClosed("tx.rollback")
	}
	tx.db.writerLifecycle.RLock()
	defer tx.db.writerLifecycle.RUnlock()
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.db.closed || tx.work == nil || tx.db.writers[tx.id] != tx {
		return txClosed("tx.rollback")
	}
	err := tx.work.writeTxn.Rollback()
	tx.db.closeNativeWriteLocked(tx)
	return err
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
	db.writeCatalog = db.catalog.Clone()
	db.writeFootprint = txn.Footprint{}
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
	rebased := false
	rebasedCatalog := db.catalog
	if dirty {
		validation, err := db.commitCoordinator().Validate(record.ParentTxnID, &db.writeFootprint)
		if err != nil {
			db.writeTxn.CommitFailed()
			db.clearWrite()
			return err
		}
		if len(validation.Conflicts) != 0 {
			db.writeTxn.CommitFailed()
			db.clearWrite()
			return dberrors.New(dberrors.CodeConflict, "db.commit", fmt.Sprintf("logical write conflict on %d resource(s) since transaction %d", len(validation.Conflicts), record.ParentTxnID))
		}
		if validation.LatestTxnID != record.ParentTxnID {
			record, rebasedCatalog, err = db.replayTransaction(ctx, db.catalogMutations, db.writeMutations)
			if err != nil {
				db.writeTxn.CommitFailed()
				db.clearWrite()
				return err
			}
			rebased = true
		}
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
	if dirty {
		if rebased {
			db.catalog = rebasedCatalog
		} else {
			db.catalog = db.writeCatalog
		}
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
	db.writeCatalog = nil
	db.writeFootprint = txn.Footprint{}
	db.writeMutations = nil
	db.catalogMutations = nil
	db.txOwner = 0
}

func (db *DB) closeNativeWriteLocked(tx *Tx) {
	delete(db.writers, tx.id)
	db.unpinSnapshot(tx.baseTxnID)
	if tx.work != nil {
		tx.work.clearWrite()
		tx.work = nil
	}
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
