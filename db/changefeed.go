package db

import (
	"context"

	"yodadb/catalog"
	"yodadb/changefeed"
	"yodadb/dberrors"
	"yodadb/storage/manifest"
)

func (db *DB) appendChangeMutations(ctx context.Context, next manifest.Manifest, schema *catalog.Catalog, tableIDs map[catalog.TableID]catalog.TableID, mutations []rowMutation) (manifest.Manifest, error) {
	if len(mutations) == 0 {
		return next, nil
	}
	changes := make([]changefeed.Change, len(mutations))
	for index, mutation := range mutations {
		tableID := mutation.tableID
		if mapped, exists := tableIDs[tableID]; exists {
			tableID = mapped
		}
		descriptor, found := schema.TableByID(tableID)
		if !found {
			return manifest.Manifest{}, dberrors.New(dberrors.CodeCorruption, "db.changefeed", "mutation table is absent from committed catalog")
		}
		change := changefeed.Change{TableID: uint64(tableID)}
		switch mutation.kind {
		case rowInsert:
			change.Kind = changefeed.Insert
			change.AfterKey = mutation.afterKey
			change.After = &changefeed.Image{SchemaVersion: descriptor.SchemaVersion, Row: mutation.after}
		case rowUpdate:
			change.Kind = changefeed.Update
			change.BeforeKey = mutation.beforeKey
			change.AfterKey = mutation.afterKey
			change.Before = &changefeed.Image{SchemaVersion: descriptor.SchemaVersion, Row: mutation.before}
			change.After = &changefeed.Image{SchemaVersion: descriptor.SchemaVersion, Row: mutation.after}
		case rowDelete:
			change.Kind = changefeed.Delete
			change.BeforeKey = mutation.beforeKey
			change.Before = &changefeed.Image{SchemaVersion: descriptor.SchemaVersion, Row: mutation.before}
		default:
			return manifest.Manifest{}, dberrors.New(dberrors.CodeCorruption, "db.changefeed", "unknown row mutation kind")
		}
		changes[index] = change
	}
	root, err := changefeed.Append(ctx, db.storage, next.ChangeRoot, next.TxnID, changes)
	if err != nil {
		return manifest.Manifest{}, err
	}
	next.ChangeRoot = root
	return next, nil
}
