package changefeed

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/page"
)

const (
	positionKeySize = 16
	chunkSize       = 4000
)

// PageStore is the immutable-page boundary required by the change log.
type PageStore interface {
	btree.PageSource
	btree.PageAppender
}

// Append adds one transaction's ordered changes and returns the new tree root.
func Append(ctx context.Context, store PageStore, root page.ID, txnID uint64, changes []Change) (page.ID, error) {
	if store == nil {
		return 0, dberrors.New(dberrors.CodeConstraint, "changefeed.append", "page store cannot be nil")
	}
	if txnID == 0 {
		return 0, dberrors.New(dberrors.CodeConstraint, "changefeed.append", "transaction ID must be nonzero")
	}
	if uint64(len(changes)) > math.MaxUint32+1 {
		return 0, dberrors.New(dberrors.CodeResourceLimit, "changefeed.append", "transaction contains too many changes")
	}
	current := root
	for index, change := range changes {
		change.TxnID = txnID
		change.Sequence = uint32(index)
		record, err := EncodeRecord(change)
		if err != nil {
			return 0, err
		}
		for offset, chunk := 0, uint32(0); offset < len(record); chunk++ {
			end := min(offset+chunkSize, len(record))
			tree, err := btree.Open(store, current)
			if err != nil {
				return 0, err
			}
			current, err = tree.Insert(ctx, store, txnID, encodePosition(txnID, uint32(index), chunk), record[offset:end])
			if err != nil {
				return 0, err
			}
			offset = end
		}
	}
	return current, nil
}

// ReadAll validates and returns every change in transaction order.
func ReadAll(ctx context.Context, source btree.PageSource, root page.ID) ([]Change, error) {
	tree, err := btree.Open(source, root)
	if err != nil {
		return nil, err
	}
	cursor, err := tree.Range(ctx, nil, nil)
	if err != nil {
		return nil, err
	}
	var changes []Change
	var currentTxn uint64
	var currentSequence uint32
	var expectedChunk uint32
	var previousChunkSize int
	var record []byte
	flush := func() error {
		if len(record) == 0 {
			return nil
		}
		change, err := DecodeRecord(currentTxn, currentSequence, record)
		if err != nil {
			return err
		}
		changes = append(changes, change)
		return nil
	}
	for {
		key, value, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			if err := flush(); err != nil {
				return nil, err
			}
			return changes, nil
		}
		if err != nil {
			return nil, err
		}
		if len(value) == 0 || len(value) > chunkSize {
			return nil, logCorruption("record chunk has invalid size")
		}
		txnID, sequence, chunk, err := decodePosition(key)
		if err != nil {
			return nil, err
		}
		if len(record) == 0 {
			if sequence != 0 || chunk != 0 {
				return nil, logCorruption("first record chunk is not zero")
			}
			currentTxn, currentSequence, expectedChunk = txnID, sequence, 0
		} else if txnID != currentTxn || sequence != currentSequence {
			if err := flush(); err != nil {
				return nil, err
			}
			if txnID < currentTxn || (txnID == currentTxn && sequence != currentSequence+1) || (txnID != currentTxn && sequence != 0) || chunk != 0 {
				return nil, logCorruption("record positions are not contiguous")
			}
			currentTxn, currentSequence, expectedChunk = txnID, sequence, 0
			previousChunkSize = 0
			record = record[:0]
		}
		if chunk != expectedChunk {
			return nil, logCorruption("record chunks are not contiguous")
		}
		if chunk != 0 && previousChunkSize != chunkSize {
			return nil, logCorruption("non-final record chunk is not full")
		}
		if len(record) > maxRecordBytes-len(value) {
			return nil, logCorruption("reassembled record exceeds size limit")
		}
		record = append(record, value...)
		previousChunkSize = len(value)
		expectedChunk++
	}
}

func encodePosition(txnID uint64, sequence, chunk uint32) []byte {
	key := make([]byte, positionKeySize)
	binary.BigEndian.PutUint64(key[0:8], txnID)
	binary.BigEndian.PutUint32(key[8:12], sequence)
	binary.BigEndian.PutUint32(key[12:16], chunk)
	return key
}

func decodePosition(key []byte) (uint64, uint32, uint32, error) {
	if len(key) != positionKeySize {
		return 0, 0, 0, logCorruption(fmt.Sprintf("position key has %d bytes, expected %d", len(key), positionKeySize))
	}
	txnID := binary.BigEndian.Uint64(key[:8])
	if txnID == 0 {
		return 0, 0, 0, logCorruption("position transaction ID is zero")
	}
	return txnID, binary.BigEndian.Uint32(key[8:12]), binary.BigEndian.Uint32(key[12:16]), nil
}

func logCorruption(message string) error {
	return dberrors.New(dberrors.CodeCorruption, "changefeed.read", message)
}
