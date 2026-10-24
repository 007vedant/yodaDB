package db

import (
	"context"
	"fmt"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/storage/manifest"
)

// NamedSnapshot describes a durable name that pins one committed transaction.
type NamedSnapshot struct {
	Name  string
	TxnID uint64
}

// CreateSnapshot durably names and pins an exact retained transaction.
func (db *DB) CreateSnapshot(ctx context.Context, name string, txnID uint64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.requireHistoryMutation(ctx, "db.create_snapshot"); err != nil {
		return err
	}
	if txnID == 0 {
		return dberrors.New(dberrors.CodeConstraint, "db.create_snapshot", "snapshot transaction ID must be nonzero")
	}
	if _, found, err := db.storage.ManifestByTxnID(txnID); err != nil {
		return err
	} else if !found {
		return dberrors.New(dberrors.CodeConstraint, "db.create_snapshot", fmt.Sprintf("transaction %d does not exist", txnID))
	}
	nextCatalog := db.catalog.Clone()
	if _, err := nextCatalog.CreateSnapshot(name, txnID); err != nil {
		return err
	}
	return db.publishCatalogMutation(ctx, nextCatalog)
}

// DropSnapshot removes a durable snapshot name. Active ReadTx handles opened
// from it continue to pin their transaction until closed.
func (db *DB) DropSnapshot(ctx context.Context, name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.requireHistoryMutation(ctx, "db.drop_snapshot"); err != nil {
		return err
	}
	nextCatalog := db.catalog.Clone()
	if err := nextCatalog.DropSnapshot(name); err != nil {
		return err
	}
	return db.publishCatalogMutation(ctx, nextCatalog)
}

// Snapshots returns durable named snapshots in creation order.
func (db *DB) Snapshots() ([]NamedSnapshot, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "db.snapshots", "database is closed")
	}
	if db.storage == nil {
		return nil, dberrors.New(dberrors.CodeUnsupported, "db.snapshots", "named snapshots require a persistent database")
	}
	stored := db.catalog.Snapshots()
	result := make([]NamedSnapshot, len(stored))
	for index, snapshot := range stored {
		result[index] = NamedSnapshot{Name: snapshot.Name, TxnID: snapshot.TxnID}
	}
	return result, nil
}

// OpenSnapshot opens a read transaction at a durable named snapshot.
func (db *DB) OpenSnapshot(ctx context.Context, name string) (*ReadTx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "db.open_snapshot", "database is closed")
	}
	if err := contextError(ctx, "db.open_snapshot"); err != nil {
		return nil, err
	}
	if db.storage == nil {
		return nil, dberrors.New(dberrors.CodeUnsupported, "db.open_snapshot", "named snapshots require a persistent database")
	}
	named, found := db.catalog.LookupSnapshot(name)
	if !found {
		return nil, dberrors.New(dberrors.CodeConstraint, "db.open_snapshot", fmt.Sprintf("unknown snapshot %q", name))
	}
	record, schema, err := db.snapshotByTxnID(ctx, named.TxnID)
	if err != nil {
		return nil, err
	}
	db.pinSnapshot(record.TxnID)
	return &ReadTx{db: db, snapshot: record, catalog: schema}, nil
}

// RetainHistoryFrom advances the logical retention boundary. It does not
// reclaim pages; compaction may later reclaim history older than the boundary.
func (db *DB) RetainHistoryFrom(ctx context.Context, oldestTxnID uint64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.requireHistoryMutation(ctx, "db.retain_history"); err != nil {
		return err
	}
	if oldestTxnID == 0 {
		return dberrors.New(dberrors.CodeConstraint, "db.retain_history", "oldest retained transaction must be nonzero")
	}
	current := db.manifest.OldestRetainedTxnID
	if current != 0 && oldestTxnID < current {
		return dberrors.New(dberrors.CodeHistoryNotRetained, "db.retain_history", fmt.Sprintf("cannot restore history before transaction %d", current))
	}
	if oldestTxnID == current {
		return nil
	}
	if _, found, err := db.storage.ManifestByTxnID(oldestTxnID); err != nil {
		return err
	} else if !found {
		return dberrors.New(dberrors.CodeConstraint, "db.retain_history", fmt.Sprintf("transaction %d does not exist", oldestTxnID))
	}
	for txnID, count := range db.readPins {
		if count != 0 && txnID != 0 && txnID < oldestTxnID {
			return dberrors.New(dberrors.CodeConflict, "db.retain_history", fmt.Sprintf("active read transaction pins transaction %d", txnID))
		}
	}
	for _, snapshot := range db.catalog.Snapshots() {
		if snapshot.TxnID < oldestTxnID {
			return dberrors.New(dberrors.CodeConflict, "db.retain_history", fmt.Sprintf("snapshot %q pins transaction %d", snapshot.Name, snapshot.TxnID))
		}
	}
	next := db.nextManifest(db.currentTxnID())
	next.OldestRetainedTxnID = oldestTxnID
	return db.publish(ctx, next)
}

func (db *DB) requireHistoryMutation(ctx context.Context, operation string) error {
	if db.closed {
		return dberrors.New(dberrors.CodeTransactionClosed, operation, "database is closed")
	}
	if err := contextError(ctx, operation); err != nil {
		return err
	}
	if db.storage == nil {
		return dberrors.New(dberrors.CodeUnsupported, operation, "history requires a persistent database")
	}
	if db.writeTxn != nil {
		return dberrors.New(dberrors.CodeConflict, operation, "a write transaction is active")
	}
	return nil
}

func (db *DB) publishCatalogMutation(ctx context.Context, nextCatalog *catalog.Catalog) error {
	txnID := db.currentTxnID()
	root, err := catalog.Save(ctx, db.storage, txnID, nextCatalog)
	if err != nil {
		return err
	}
	next := db.nextManifest(txnID)
	next.CatalogRoot = root
	if err := db.publish(ctx, next); err != nil {
		return err
	}
	db.catalog = nextCatalog
	return nil
}

func validateCatalogSnapshots(storage *manifest.Database, record manifest.Manifest, schema *catalog.Catalog) error {
	for _, snapshot := range schema.Snapshots() {
		if snapshot.TxnID > record.TxnID || (record.OldestRetainedTxnID != 0 && snapshot.TxnID < record.OldestRetainedTxnID) {
			return dberrors.New(dberrors.CodeCorruption, "db.open", fmt.Sprintf("snapshot %q targets unretained transaction %d", snapshot.Name, snapshot.TxnID))
		}
		if _, found, err := storage.ManifestByTxnID(snapshot.TxnID); err != nil {
			return dberrors.Wrap(err, dberrors.CodeCorruption, "db.open", fmt.Sprintf("validate snapshot %q", snapshot.Name))
		} else if !found {
			return dberrors.New(dberrors.CodeCorruption, "db.open", fmt.Sprintf("snapshot %q targets missing transaction %d", snapshot.Name, snapshot.TxnID))
		}
	}
	return nil
}
