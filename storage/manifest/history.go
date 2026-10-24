package manifest

import (
	"fmt"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

type historyEntry struct {
	pageID page.ID
	record Manifest
}

// History returns retained committed manifests in oldest-to-newest order.
func (d *Database) History() ([]Manifest, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries, err := d.historyNewestFirstLocked()
	if err != nil {
		return nil, err
	}
	result := make([]Manifest, len(entries))
	for index := range entries {
		result[len(entries)-1-index] = entries[index].record
	}
	return result, nil
}

// ManifestByTxnID returns the retained committed manifest with the exact
// transaction ID. Found is false for zero, future, or unretained IDs.
func (d *Database) ManifestByTxnID(txnID uint64) (record Manifest, found bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if txnID == 0 || txnID > d.metadata.LatestTxnID ||
		(d.metadata.OldestRetainedTxnID != 0 && txnID < d.metadata.OldestRetainedTxnID) {
		return Manifest{}, false, nil
	}
	entries, err := d.historyNewestFirstLocked()
	if err != nil {
		return Manifest{}, false, err
	}
	for _, entry := range entries {
		if entry.record.TxnID == txnID {
			return entry.record, true, nil
		}
		if entry.record.TxnID < txnID {
			break
		}
	}
	return Manifest{}, false, nil
}

func (d *Database) historyNewestFirstLocked() ([]historyEntry, error) {
	if d.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "database.history", "database is closed")
	}
	if d.metadata.LatestManifestPageID == 0 {
		return nil, nil
	}
	current, err := d.readManifestLocked(d.metadata.LatestManifestPageID, d.metadata.LatestTxnID)
	if err != nil {
		return nil, err
	}
	entries := make([]historyEntry, 0)
	for {
		entries = append(entries, current)
		parentTxnID := current.record.ParentTxnID
		if parentTxnID == 0 || (d.metadata.OldestRetainedTxnID != 0 && parentTxnID < d.metadata.OldestRetainedTxnID) {
			return entries, nil
		}
		current, err = d.findManifestBeforeLocked(current.pageID, parentTxnID)
		if err != nil {
			return nil, err
		}
	}
}

func (d *Database) findManifestBeforeLocked(before page.ID, txnID uint64) (historyEntry, error) {
	for candidate := before; candidate > 2; {
		candidate--
		handle, err := d.pager.Read(candidate)
		if err != nil {
			continue
		}
		stored := handle.Page()
		handle.Release()
		if stored.Header.Type != page.TypeManifest || stored.Header.Generation != txnID {
			continue
		}
		record, err := DecodeManifest(stored.Payload)
		if err != nil {
			return historyEntry{}, dberrors.Wrap(err, dberrors.CodeCorruption, "database.history", fmt.Sprintf("decode manifest page %d", candidate))
		}
		if record.TxnID != txnID {
			return historyEntry{}, dberrors.New(dberrors.CodeCorruption, "database.history", fmt.Sprintf("manifest page %d transaction disagrees with generation", candidate))
		}
		return historyEntry{pageID: candidate, record: record}, nil
	}
	return historyEntry{}, dberrors.New(dberrors.CodeCorruption, "database.history", fmt.Sprintf("committed transaction %d has no predecessor manifest", txnID))
}

func (d *Database) readManifestLocked(id page.ID, txnID uint64) (historyEntry, error) {
	handle, err := d.pager.Read(id)
	if err != nil {
		return historyEntry{}, err
	}
	defer handle.Release()
	stored := handle.Page()
	if stored.Header.Type != page.TypeManifest {
		return historyEntry{}, dberrors.New(dberrors.CodeCorruption, "database.history", fmt.Sprintf("page %d is %s, expected manifest", id, stored.Header.Type))
	}
	if stored.Header.Generation != txnID {
		return historyEntry{}, dberrors.New(dberrors.CodeCorruption, "database.history", fmt.Sprintf("manifest page %d generation %d, expected transaction %d", id, stored.Header.Generation, txnID))
	}
	record, err := DecodeManifest(stored.Payload)
	if err != nil {
		return historyEntry{}, dberrors.Wrap(err, dberrors.CodeCorruption, "database.history", fmt.Sprintf("decode manifest page %d", id))
	}
	if record.TxnID != txnID {
		return historyEntry{}, dberrors.New(dberrors.CodeCorruption, "database.history", fmt.Sprintf("manifest page %d transaction disagrees with generation", id))
	}
	return historyEntry{pageID: id, record: record}, nil
}
