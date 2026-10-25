package changefeed

import (
	"bytes"

	"yodadb/types"
)

// Kind identifies one logical row mutation.
type Kind uint8

const (
	Insert Kind = iota + 1
	Update
	Delete
)

// Image is a row encoded under one table schema version.
type Image struct {
	SchemaVersion uint64
	Row           types.Row
}

// Change is one committed logical row mutation.
type Change struct {
	TxnID     uint64
	Sequence  uint32
	TableID   uint64
	Kind      Kind
	BeforeKey []byte
	AfterKey  []byte
	Before    *Image
	After     *Image
}

func cloneChange(change Change) Change {
	change.BeforeKey = bytes.Clone(change.BeforeKey)
	change.AfterKey = bytes.Clone(change.AfterKey)
	change.Before = cloneImage(change.Before)
	change.After = cloneImage(change.After)
	return change
}

func cloneImage(image *Image) *Image {
	if image == nil {
		return nil
	}
	return &Image{SchemaVersion: image.SchemaVersion, Row: append(types.Row(nil), image.Row...)}
}
