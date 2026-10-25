package db

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"yodadb/catalog"
	"yodadb/dberrors"
	storageindex "yodadb/storage/index"
	"yodadb/storage/manifest"
	"yodadb/types"
)

type rowMutationKind uint8

const (
	rowInsert rowMutationKind = iota + 1
	rowUpdate
	rowDelete
)

// rowMutation is the exact logical effect prepared against a snapshot. Exact
// rows avoid re-evaluating predicates against newer data during rebase.
type rowMutation struct {
	kind      rowMutationKind
	tableID   catalog.TableID
	beforeKey []byte
	afterKey  []byte
	before    types.Row
	after     types.Row
}

func newInsertMutation(tableID catalog.TableID, key []byte, row types.Row) rowMutation {
	return rowMutation{kind: rowInsert, tableID: tableID, afterKey: bytes.Clone(key), after: cloneRow(row)}
}

func newUpdateMutation(tableID catalog.TableID, beforeKey, afterKey []byte, before, after types.Row) rowMutation {
	return rowMutation{
		kind:      rowUpdate,
		tableID:   tableID,
		beforeKey: bytes.Clone(beforeKey),
		afterKey:  bytes.Clone(afterKey),
		before:    cloneRow(before),
		after:     cloneRow(after),
	}
}

func newDeleteMutation(tableID catalog.TableID, key []byte, row types.Row) rowMutation {
	return rowMutation{kind: rowDelete, tableID: tableID, beforeKey: bytes.Clone(key), before: cloneRow(row)}
}

func (db *DB) replayRowMutations(ctx context.Context, mutations []rowMutation) (manifest.Manifest, error) {
	if db.manifest.TxnID == ^uint64(0) {
		return manifest.Manifest{}, dberrors.New(dberrors.CodeResourceLimit, "db.replay", "transaction ID space is exhausted")
	}
	next := cloneManifest(db.manifest)
	next.TxnID = db.manifest.TxnID + 1
	next.ParentTxnID = db.manifest.TxnID
	next.CommitUnixNano = time.Now().UnixNano()

	for _, mutation := range mutations {
		descriptor, found := db.catalog.TableByID(mutation.tableID)
		if !found {
			return manifest.Manifest{}, replayConflict(mutation, "table no longer exists")
		}
		tableRoot := next.TableRoots[uint64(mutation.tableID)]
		tableTree, err := db.openTable(descriptor, tableRoot)
		if err != nil {
			return manifest.Manifest{}, err
		}

		switch mutation.kind {
		case rowInsert:
			hiddenID, err := hiddenRowID(descriptor, mutation.afterKey)
			if err != nil {
				return manifest.Manifest{}, err
			}
			newRoot, key, err := tableTree.Insert(ctx, db.storage, next.TxnID, hiddenID, mutation.after)
			if err != nil {
				return manifest.Manifest{}, dberrors.Wrap(err, dberrors.CodeConflict, "db.replay", "insert no longer applies to latest roots")
			}
			if !bytes.Equal(key, mutation.afterKey) {
				return manifest.Manifest{}, replayConflict(mutation, "insert key changed")
			}
			next.TableRoots[uint64(mutation.tableID)] = newRoot
			if err := db.replayInsertIndexes(ctx, next.TxnID, descriptor, mutation.after, key, &next); err != nil {
				return manifest.Manifest{}, err
			}

		case rowUpdate:
			current, found, err := tableTree.Lookup(ctx, mutation.beforeKey)
			if err != nil {
				return manifest.Manifest{}, err
			}
			if !found || !rowsEqual(current, mutation.before) {
				return manifest.Manifest{}, replayConflict(mutation, "updated row changed after validation")
			}
			if err := db.replayDeleteIndexes(ctx, next.TxnID, descriptor, current, mutation.beforeKey, &next); err != nil {
				return manifest.Manifest{}, err
			}
			hiddenID, err := hiddenRowID(descriptor, mutation.beforeKey)
			if err != nil {
				return manifest.Manifest{}, err
			}
			newRoot, key, err := tableTree.Replace(ctx, db.storage, next.TxnID, hiddenID, mutation.beforeKey, mutation.after)
			if err != nil {
				return manifest.Manifest{}, dberrors.Wrap(err, dberrors.CodeConflict, "db.replay", "update no longer applies to latest roots")
			}
			if !bytes.Equal(key, mutation.afterKey) {
				return manifest.Manifest{}, replayConflict(mutation, "updated key changed")
			}
			next.TableRoots[uint64(mutation.tableID)] = newRoot
			if err := db.replayInsertIndexes(ctx, next.TxnID, descriptor, mutation.after, key, &next); err != nil {
				return manifest.Manifest{}, err
			}

		case rowDelete:
			current, found, err := tableTree.Lookup(ctx, mutation.beforeKey)
			if err != nil {
				return manifest.Manifest{}, err
			}
			if !found || !rowsEqual(current, mutation.before) {
				return manifest.Manifest{}, replayConflict(mutation, "deleted row changed after validation")
			}
			if err := db.replayDeleteIndexes(ctx, next.TxnID, descriptor, current, mutation.beforeKey, &next); err != nil {
				return manifest.Manifest{}, err
			}
			newRoot, deleted, err := tableTree.Delete(ctx, db.storage, next.TxnID, mutation.beforeKey)
			if err != nil {
				return manifest.Manifest{}, err
			}
			if !deleted {
				return manifest.Manifest{}, replayConflict(mutation, "row disappeared during delete replay")
			}
			next.TableRoots[uint64(mutation.tableID)] = newRoot

		default:
			return manifest.Manifest{}, replayConflict(mutation, "unknown mutation kind")
		}
	}
	return next, nil
}

func (db *DB) replayInsertIndexes(ctx context.Context, txnID uint64, descriptor catalog.Table, row types.Row, key []byte, next *manifest.Manifest) error {
	for _, index := range db.catalog.IndexesForTable(descriptor.ID) {
		definition, err := indexDefinition(descriptor, index)
		if err != nil {
			return err
		}
		tree, err := storageindex.Open(db.storage, next.IndexRoots[uint64(index.ID)], definition)
		if err != nil {
			return err
		}
		root, err := tree.Insert(ctx, db.storage, txnID, row, key)
		if err != nil {
			return dberrors.Wrap(err, dberrors.CodeConflict, "db.replay", fmt.Sprintf("index %q insert conflicts with latest roots", index.Name))
		}
		next.IndexRoots[uint64(index.ID)] = root
	}
	return nil
}

func (db *DB) replayDeleteIndexes(ctx context.Context, txnID uint64, descriptor catalog.Table, row types.Row, key []byte, next *manifest.Manifest) error {
	for _, index := range db.catalog.IndexesForTable(descriptor.ID) {
		root, err := db.deleteIndexEntry(ctx, txnID, descriptor, index, next.IndexRoots[uint64(index.ID)], row, key)
		if err != nil {
			return dberrors.Wrap(err, dberrors.CodeConflict, "db.replay", fmt.Sprintf("index %q delete conflicts with latest roots", index.Name))
		}
		next.IndexRoots[uint64(index.ID)] = root
	}
	return nil
}

func replayConflict(mutation rowMutation, message string) error {
	return dberrors.New(dberrors.CodeConflict, "db.replay", fmt.Sprintf("table %d: %s", mutation.tableID, message))
}

func cloneRow(row types.Row) types.Row { return append(types.Row(nil), row...) }

func rowsEqual(left, right types.Row) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		comparison, err := types.Compare(left[index], right[index])
		if err != nil || comparison != 0 {
			return false
		}
	}
	return true
}
