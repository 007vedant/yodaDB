package db

import (
	"fmt"

	"yodadb/dberrors"
	"yodadb/txn"
)

// ConflictKind describes the commit phase that rejected a transaction.
type ConflictKind string

const (
	// ConflictValidation means conflict history could not validate the stale base.
	ConflictValidation ConflictKind = "validation"
	// ConflictWriteWrite means another transaction wrote the same logical resource.
	ConflictWriteWrite ConflictKind = "write_write"
	// ConflictRebase means exact mutation replay no longer applied to the latest state.
	ConflictRebase ConflictKind = "rebase"
)

// ConflictError describes a retryable logical conflict detected during commit.
// Resources is empty when validation could not identify an exact overlap.
type ConflictError struct {
	Kind        ConflictKind
	BaseTxnID   uint64
	LatestTxnID uint64
	Resources   []txn.Resource
	cause       error
}

func newConflictError(kind ConflictKind, operation string, baseTxnID, latestTxnID uint64, resources []txn.Resource, cause error) *ConflictError {
	if cause == nil {
		message := fmt.Sprintf("%s conflict since transaction %d", kind, baseTxnID)
		if len(resources) != 0 {
			message = fmt.Sprintf("logical write conflict on %d resource(s) since transaction %d", len(resources), baseTxnID)
		}
		cause = dberrors.New(dberrors.CodeConflict, operation, message)
	}
	return &ConflictError{
		Kind:        kind,
		BaseTxnID:   baseTxnID,
		LatestTxnID: latestTxnID,
		Resources:   append([]txn.Resource(nil), resources...),
		cause:       cause,
	}
}

// Error implements error.
func (e *ConflictError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return "transaction conflict"
}

// Unwrap preserves compatibility with dberrors.ErrConflict.
func (e *ConflictError) Unwrap() error {
	if e == nil || e.cause == nil {
		return dberrors.ErrConflict
	}
	return e.cause
}

func commitConflict(kind ConflictKind, operation string, baseTxnID, latestTxnID uint64, resources []txn.Resource, cause error) error {
	if cause != nil && dberrors.CodeOf(cause) != dberrors.CodeConflict {
		return cause
	}
	return newConflictError(kind, operation, baseTxnID, latestTxnID, resources, cause)
}
