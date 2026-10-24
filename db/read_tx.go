package db

import (
	"context"
	"fmt"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/plan"
	"yodadb/storage/manifest"
	"yodadb/types"
)

// ReadTx is a read-only transaction bound to one committed manifest and
// catalog snapshot. Its results do not change when later writes commit.
type ReadTx struct {
	db       *DB
	snapshot manifest.Manifest
	catalog  *catalog.Catalog
	closed   bool
}

// BeginRead starts a read-only transaction at the latest committed snapshot.
func (db *DB) BeginRead(ctx context.Context) (*ReadTx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "db.begin_read", "database is closed")
	}
	if err := contextError(ctx, "db.begin_read"); err != nil {
		return nil, err
	}
	if db.storage == nil {
		return nil, dberrors.New(dberrors.CodeUnsupported, "db.begin_read", "read transactions require a persistent database")
	}
	snapshot := cloneManifest(db.manifest)
	db.pinSnapshot(snapshot.TxnID)
	return &ReadTx{db: db, snapshot: snapshot, catalog: db.catalog.Clone()}, nil
}

// TxnID returns the committed transaction visible to the read transaction.
// Zero identifies an empty database with no committed manifest.
func (tx *ReadTx) TxnID() uint64 {
	if tx == nil {
		return 0
	}
	return tx.snapshot.TxnID
}

// Query executes SELECT or EXPLAIN against the pinned snapshot.
func (tx *ReadTx) Query(ctx context.Context, sql string, arguments ...any) (*Rows, error) {
	if tx == nil || tx.db == nil {
		return nil, txClosed("read_tx.query")
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	if tx.closed || tx.db.closed {
		return nil, txClosed("read_tx.query")
	}
	statement, err := tx.db.prepareWithCatalog(sql, arguments, tx.catalog)
	if err != nil {
		return nil, err
	}
	switch statement := statement.(type) {
	case *binder.Select:
		outcome, err := tx.db.selectPersistentAt(ctx, statement, tx.snapshot, tx.catalog)
		return outcome.Rows, err
	case *binder.Explain:
		access := plan.ChooseSelect(statement.Select, tx.catalog)
		return newRows([]string{"plan"}, []types.Row{{types.Text(access.Description)}}), nil
	default:
		return nil, dberrors.New(dberrors.CodeUnsupported, "read_tx.query", fmt.Sprintf("%T does not return rows", statement))
	}
}

// Close releases the snapshot pin. It is idempotent.
func (tx *ReadTx) Close() error {
	if tx == nil || tx.db == nil {
		return nil
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	if tx.closed {
		return nil
	}
	tx.closed = true
	tx.db.unpinSnapshot(tx.snapshot.TxnID)
	return nil
}

func (db *DB) pinSnapshot(txnID uint64) {
	if db.readPins == nil {
		db.readPins = make(map[uint64]uint64)
	}
	db.readPins[txnID]++
}

func (db *DB) unpinSnapshot(txnID uint64) {
	count := db.readPins[txnID]
	if count <= 1 {
		delete(db.readPins, txnID)
		return
	}
	db.readPins[txnID] = count - 1
}
