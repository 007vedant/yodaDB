package db

import (
	"context"
	"fmt"
	"time"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	storageindex "yodadb/storage/index"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
	"yodadb/txn"
)

type catalogMutationKind uint8

const (
	catalogCreateTable catalogMutationKind = iota + 1
	catalogCreateIndex
)

type catalogMutation struct {
	kind     catalogMutationKind
	tableID  catalog.TableID
	indexID  catalog.IndexID
	name     string
	columns  []catalog.ColumnSpec
	ordinals []int
	unique   bool
}

func (db *DB) recordCreateTable(statement *binder.CreateTable, created catalog.Table) error {
	resource, err := txn.NewCatalogNameResource(txn.CatalogTable, 0, statement.Name)
	if err != nil {
		return err
	}
	if err := db.writeFootprint.RecordWrite(resource); err != nil {
		return err
	}
	db.catalogMutations = append(db.catalogMutations, catalogMutation{
		kind:    catalogCreateTable,
		tableID: created.ID,
		name:    statement.Name,
		columns: append([]catalog.ColumnSpec(nil), statement.Columns...),
	})
	return nil
}

func (db *DB) recordCreateIndex(statement *binder.CreateIndex, created catalog.Index) error {
	if !db.isLocallyCreatedTable(statement.Table.ID) {
		resource, err := txn.NewCatalogNameResource(txn.CatalogIndex, uint64(statement.Table.ID), statement.Name)
		if err != nil {
			return err
		}
		if err := db.writeFootprint.RecordWrite(resource); err != nil {
			return err
		}
	}
	ordinals := make([]int, len(statement.ColumnIDs))
	for position, columnID := range statement.ColumnIDs {
		ordinals[position] = -1
		for ordinal, column := range statement.Table.Columns {
			if column.ID == columnID {
				ordinals[position] = ordinal
				break
			}
		}
		if ordinals[position] < 0 {
			return dberrors.New(dberrors.CodeCorruption, "db.record_catalog", "index column is absent from its table")
		}
	}
	db.catalogMutations = append(db.catalogMutations, catalogMutation{
		kind:     catalogCreateIndex,
		tableID:  statement.Table.ID,
		indexID:  created.ID,
		name:     statement.Name,
		ordinals: ordinals,
		unique:   statement.Unique,
	})
	return nil
}

func (db *DB) isLocallyCreatedTable(tableID catalog.TableID) bool {
	for _, mutation := range db.catalogMutations {
		if mutation.kind == catalogCreateTable && mutation.tableID == tableID {
			return true
		}
	}
	return false
}

func (db *DB) isLocallyCreatedIndex(indexID catalog.IndexID) bool {
	for _, mutation := range db.catalogMutations {
		if mutation.kind == catalogCreateIndex && mutation.indexID == indexID {
			return true
		}
	}
	return false
}

func (db *DB) replayTransaction(ctx context.Context, catalogMutations []catalogMutation, rowMutations []rowMutation) (manifest.Manifest, *catalog.Catalog, map[catalog.TableID]catalog.TableID, error) {
	if db.manifest.TxnID == ^uint64(0) {
		return manifest.Manifest{}, nil, nil, dberrors.New(dberrors.CodeResourceLimit, "db.replay", "transaction ID space is exhausted")
	}
	next := cloneManifest(db.manifest)
	next.TxnID = db.manifest.TxnID + 1
	next.ParentTxnID = db.manifest.TxnID
	next.CommitUnixNano = time.Now().UnixNano()
	schema := db.catalog.Clone()
	tableIDs := make(map[catalog.TableID]catalog.TableID)

	for _, mutation := range catalogMutations {
		switch mutation.kind {
		case catalogCreateTable:
			created, err := schema.CreateTable(mutation.name, mutation.columns)
			if err != nil {
				return manifest.Manifest{}, nil, nil, dberrors.Wrap(err, dberrors.CodeConflict, "db.replay_catalog", fmt.Sprintf("create table %q conflicts with latest catalog", mutation.name))
			}
			tableIDs[mutation.tableID] = created.ID
			next.TableRoots[uint64(created.ID)] = 0

		case catalogCreateIndex:
			tableID := mutation.tableID
			if mapped, exists := tableIDs[tableID]; exists {
				tableID = mapped
			}
			tableDescriptor, found := schema.TableByID(tableID)
			if !found {
				return manifest.Manifest{}, nil, nil, dberrors.New(dberrors.CodeConflict, "db.replay_catalog", fmt.Sprintf("table %d for index %q no longer exists", tableID, mutation.name))
			}
			columnIDs := make([]catalog.ColumnID, len(mutation.ordinals))
			for index, ordinal := range mutation.ordinals {
				if ordinal < 0 || ordinal >= len(tableDescriptor.Columns) {
					return manifest.Manifest{}, nil, nil, dberrors.New(dberrors.CodeConflict, "db.replay_catalog", fmt.Sprintf("column %d for index %q no longer exists", ordinal, mutation.name))
				}
				columnIDs[index] = tableDescriptor.Columns[ordinal].ID
			}
			created, err := schema.CreateIndex(tableID, mutation.name, columnIDs, mutation.unique)
			if err != nil {
				return manifest.Manifest{}, nil, nil, dberrors.Wrap(err, dberrors.CodeConflict, "db.replay_catalog", fmt.Sprintf("create index %q conflicts with latest catalog", mutation.name))
			}
			root, err := db.buildIndexAt(ctx, next.TxnID, next, tableDescriptor, created)
			if err != nil {
				return manifest.Manifest{}, nil, nil, dberrors.Wrap(err, dberrors.CodeConflict, "db.replay_catalog", fmt.Sprintf("build index %q against latest rows", mutation.name))
			}
			next.IndexRoots[uint64(created.ID)] = root

		default:
			return manifest.Manifest{}, nil, nil, dberrors.New(dberrors.CodeConflict, "db.replay_catalog", "unknown catalog mutation")
		}
	}

	var err error
	next, err = db.replayRowMutationsOnto(ctx, next, schema, tableIDs, rowMutations)
	if err != nil {
		return manifest.Manifest{}, nil, nil, err
	}
	if len(catalogMutations) != 0 {
		root, err := catalog.Save(ctx, db.storage, next.TxnID, schema)
		if err != nil {
			return manifest.Manifest{}, nil, nil, err
		}
		next.CatalogRoot = root
	}
	return next, schema, tableIDs, nil
}

func (db *DB) buildIndexAt(ctx context.Context, txnID uint64, current manifest.Manifest, tableDescriptor catalog.Table, descriptor catalog.Index) (page.ID, error) {
	definition, err := indexDefinition(tableDescriptor, descriptor)
	if err != nil {
		return 0, err
	}
	tableTree, err := db.openTable(tableDescriptor, current.TableRoots[uint64(tableDescriptor.ID)])
	if err != nil {
		return 0, err
	}
	records, err := tableTree.Scan(ctx)
	if err != nil {
		return 0, err
	}
	root := page.ID(0)
	for _, record := range records {
		tree, err := storageindex.Open(db.storage, root, definition)
		if err != nil {
			return 0, err
		}
		root, err = tree.Insert(ctx, db.storage, txnID, record.Row, record.Key)
		if err != nil {
			return 0, err
		}
	}
	return root, nil
}
