package txn

import (
	"fmt"
	"sync"

	"yodadb/dberrors"
)

// Validation describes write/write conflicts committed after a transaction's
// base snapshot.
type Validation struct {
	LatestTxnID uint64
	Conflicts   []Resource
}

// Coordinator retains in-process logical footprints needed to validate live
// writers. Transactions cannot survive a database reopen, so history before
// baselineTxnID is intentionally not reconstructed from disk.
type Coordinator struct {
	mu            sync.Mutex
	baselineTxnID uint64
	latestTxnID   uint64
	committed     map[uint64]Footprint
}

// NewCoordinator starts a coordinator at the latest transaction visible when
// a database handle is created or opened.
func NewCoordinator(latestTxnID uint64) *Coordinator {
	return &Coordinator{
		baselineTxnID: latestTxnID,
		latestTxnID:   latestTxnID,
		committed:     make(map[uint64]Footprint),
	}
}

// RecordCommitted stores an immutable footprint for the next committed
// transaction. The database commit mutex guarantees sequential calls.
func (c *Coordinator) RecordCommitted(txnID uint64, footprint *Footprint) error {
	if c == nil {
		return dberrors.New(dberrors.CodeTransactionClosed, "txn.coordinator.record", "commit coordinator is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if txnID != c.latestTxnID+1 {
		return dberrors.New(dberrors.CodeConflict, "txn.coordinator.record", fmt.Sprintf("transaction %d does not follow %d", txnID, c.latestTxnID))
	}
	c.committed[txnID] = footprint.Clone()
	c.latestTxnID = txnID
	return nil
}

// Validate compares candidate writes with every footprint committed after
// baseTxnID. An empty conflict list means logical validation succeeded.
func (c *Coordinator) Validate(baseTxnID uint64, candidate *Footprint) (Validation, error) {
	if c == nil {
		return Validation{}, dberrors.New(dberrors.CodeTransactionClosed, "txn.coordinator.validate", "commit coordinator is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if baseTxnID < c.baselineTxnID {
		return Validation{}, dberrors.New(dberrors.CodeConflict, "txn.coordinator.validate", fmt.Sprintf("transaction base %d predates available conflict history %d", baseTxnID, c.baselineTxnID))
	}
	if baseTxnID > c.latestTxnID {
		return Validation{}, dberrors.New(dberrors.CodeConstraint, "txn.coordinator.validate", fmt.Sprintf("transaction base %d exceeds latest transaction %d", baseTxnID, c.latestTxnID))
	}
	var committed Footprint
	for txnID := baseTxnID + 1; txnID <= c.latestTxnID; txnID++ {
		footprint, exists := c.committed[txnID]
		if !exists {
			return Validation{}, dberrors.New(dberrors.CodeConflict, "txn.coordinator.validate", fmt.Sprintf("conflict history for transaction %d is unavailable", txnID))
		}
		committed.Merge(&footprint)
	}
	return Validation{LatestTxnID: c.latestTxnID, Conflicts: candidate.WriteConflicts(&committed)}, nil
}
