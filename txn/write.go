package txn

import (
	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

// State is the lifecycle state of one write transaction.
type State uint8

const (
	// Active accepts reads and writes against the private root set.
	Active State = iota
	// Committing has stopped accepting work and is publishing.
	Committing
	// Committed finished successfully.
	Committed
	// Aborted was rolled back or failed during commit.
	Aborted
)

// Write owns the private manifest root set of a single active writer. Pages may
// be appended while it is active, but its roots are unreachable until commit.
type Write struct {
	state   State
	base    manifest.Manifest
	working manifest.Manifest
	dirty   bool
}

// BeginWrite starts a write transaction from one committed manifest snapshot.
// Its transaction ID is reserved as base+1 for every private COW page.
func BeginWrite(base manifest.Manifest) (*Write, error) {
	if base.TxnID == ^uint64(0) {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "txn.begin", "transaction ID space is exhausted")
	}
	base = cloneManifest(base)
	working := cloneManifest(base)
	working.TxnID = base.TxnID + 1
	working.ParentTxnID = base.TxnID
	working.CommitUnixNano = 0
	return &Write{state: Active, base: base, working: working}, nil
}

// State returns the current lifecycle state.
func (w *Write) State() State {
	if w == nil {
		return Aborted
	}
	return w.state
}

// TxnID returns the reserved commit ID.
func (w *Write) TxnID() uint64 {
	if w == nil {
		return 0
	}
	return w.working.TxnID
}

// Working returns a defensive copy of the private root set.
func (w *Write) Working() (manifest.Manifest, error) {
	if w == nil || w.state != Active {
		return manifest.Manifest{}, closed("txn.working")
	}
	return cloneManifest(w.working), nil
}

// Advance replaces the private root set after one statement succeeds.
func (w *Write) Advance(next manifest.Manifest) error {
	if w == nil || w.state != Active {
		return closed("txn.advance")
	}
	if next.TxnID != w.working.TxnID || next.ParentTxnID != w.base.TxnID {
		return dberrors.New(dberrors.CodeConflict, "txn.advance", "private manifest does not extend the transaction base")
	}
	w.working = cloneManifest(next)
	w.dirty = true
	return nil
}

// PrepareCommit transitions to Committing and returns the root set to publish.
// Dirty is false for a read-only transaction, which needs no manifest.
func (w *Write) PrepareCommit() (record manifest.Manifest, dirty bool, err error) {
	if w == nil || w.state != Active {
		return manifest.Manifest{}, false, closed("txn.prepare_commit")
	}
	w.state = Committing
	return cloneManifest(w.working), w.dirty, nil
}

// CommitSucceeded completes a prepared transaction.
func (w *Write) CommitSucceeded() error {
	if w == nil || w.state != Committing {
		return closed("txn.commit_succeeded")
	}
	w.state = Committed
	return nil
}

// CommitFailed aborts a transaction whose publication failed.
func (w *Write) CommitFailed() {
	if w != nil && w.state == Committing {
		w.state = Aborted
	}
}

// Rollback aborts an active transaction. Its appended pages remain unreachable.
func (w *Write) Rollback() error {
	if w == nil || w.state != Active {
		return closed("txn.rollback")
	}
	w.state = Aborted
	return nil
}

func closed(operation string) error {
	return dberrors.New(dberrors.CodeTransactionClosed, operation, "transaction is not active")
}

func cloneManifest(record manifest.Manifest) manifest.Manifest {
	cloned := record
	cloned.TableRoots = make(map[uint64]page.ID, len(record.TableRoots))
	for id, root := range record.TableRoots {
		cloned.TableRoots[id] = root
	}
	cloned.IndexRoots = make(map[uint64]page.ID, len(record.IndexRoots))
	for id, root := range record.IndexRoots {
		cloned.IndexRoots[id] = root
	}
	return cloned
}
