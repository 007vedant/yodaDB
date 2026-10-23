package page

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"yodadb/dberrors"
)

const (
	leafFormatVersion byte = 1
	leafHeaderSize         = 16
	leafSlotSize           = 8
)

// LeafEntry is one key/value record in logical slot order.
type LeafEntry struct {
	Key   []byte
	Value []byte
}

// Leaf is a decoded slotted B+ tree leaf payload.
type Leaf struct {
	RightSibling ID
	Entries      []LeafEntry
}

// LeafBuilder owns a mutable, uncommitted leaf representation.
type LeafBuilder struct {
	rightSibling ID
	entries      []LeafEntry
}

// NewLeafBuilder creates an empty leaf builder.
func NewLeafBuilder(rightSibling ID) *LeafBuilder {
	return &LeafBuilder{rightSibling: rightSibling}
}

// NewLeafBuilderFromDecoded creates a mutable copy of a decoded leaf.
func NewLeafBuilderFromDecoded(leaf Leaf) *LeafBuilder {
	builder := NewLeafBuilder(leaf.RightSibling)
	for _, entry := range leaf.Entries {
		builder.entries = append(builder.entries, cloneLeafEntry(entry))
	}
	return builder
}

// Count returns the number of logical slots.
func (b *LeafBuilder) Count() int { return len(b.entries) }

// UsedBytes returns the encoded header, slots, and record bytes before the
// payload's zero-filled free space. It is useful for choosing balanced splits.
func (b *LeafBuilder) UsedBytes() int { return b.encodedSize() }

// RightSibling returns the optional right-sibling page ID.
func (b *LeafBuilder) RightSibling() ID { return b.rightSibling }

// SetRightSibling updates the builder's uncommitted sibling reference.
func (b *LeafBuilder) SetRightSibling(id ID) { b.rightSibling = id }

// Entry returns a defensive copy of one logical entry.
func (b *LeafBuilder) Entry(index int) (LeafEntry, error) {
	if index < 0 || index >= len(b.entries) {
		return LeafEntry{}, dberrors.New(dberrors.CodeConstraint, "leaf.entry", fmt.Sprintf("slot %d outside [0,%d)", index, len(b.entries)))
	}
	return cloneLeafEntry(b.entries[index]), nil
}

// Insert adds a key/value entry at a logical slot position.
func (b *LeafBuilder) Insert(index int, key, value []byte) error {
	if index < 0 || index > len(b.entries) {
		return dberrors.New(dberrors.CodeConstraint, "leaf.insert", fmt.Sprintf("slot %d outside [0,%d]", index, len(b.entries)))
	}
	entry := LeafEntry{Key: bytes.Clone(key), Value: bytes.Clone(value)}
	newSize := b.encodedSize() + leafSlotSize + uvarintLength(uint64(len(key))) + len(key) + len(value)
	if newSize > MaxPayloadSize {
		return dberrors.New(dberrors.CodeResourceLimit, "leaf.insert", fmt.Sprintf("entry would require %d-byte payload, maximum is %d", newSize, MaxPayloadSize))
	}
	b.entries = append(b.entries, LeafEntry{})
	copy(b.entries[index+1:], b.entries[index:])
	b.entries[index] = entry
	return nil
}

// Delete removes one slot from the uncommitted builder.
func (b *LeafBuilder) Delete(index int) error {
	if index < 0 || index >= len(b.entries) {
		return dberrors.New(dberrors.CodeConstraint, "leaf.delete", fmt.Sprintf("slot %d outside [0,%d)", index, len(b.entries)))
	}
	copy(b.entries[index:], b.entries[index+1:])
	b.entries[len(b.entries)-1] = LeafEntry{}
	b.entries = b.entries[:len(b.entries)-1]
	return nil
}

// Encode returns the canonical full-size version-1 leaf payload.
func (b *LeafBuilder) Encode() ([]byte, error) {
	if b.encodedSize() > MaxPayloadSize {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "leaf.encode", "leaf exceeds page payload capacity")
	}
	payload := make([]byte, MaxPayloadSize)
	payload[0] = leafFormatVersion
	binary.LittleEndian.PutUint16(payload[2:4], uint16(len(b.entries)))
	freeStart := leafHeaderSize + len(b.entries)*leafSlotSize
	freeEnd := len(payload)
	binary.LittleEndian.PutUint16(payload[4:6], uint16(freeStart))
	binary.LittleEndian.PutUint64(payload[8:16], uint64(b.rightSibling))

	for index, entry := range b.entries {
		record := make([]byte, 0, uvarintLength(uint64(len(entry.Key)))+len(entry.Key)+len(entry.Value))
		record = binary.AppendUvarint(record, uint64(len(entry.Key)))
		record = append(record, entry.Key...)
		record = append(record, entry.Value...)
		freeEnd -= len(record)
		copy(payload[freeEnd:], record)
		slotOffset := leafHeaderSize + index*leafSlotSize
		binary.LittleEndian.PutUint16(payload[slotOffset:slotOffset+2], uint16(freeEnd))
		binary.LittleEndian.PutUint16(payload[slotOffset+2:slotOffset+4], uint16(len(record)))
	}
	binary.LittleEndian.PutUint16(payload[6:8], uint16(freeEnd))
	return payload, nil
}

// DecodeLeaf validates and decodes a canonical version-1 leaf payload.
func DecodeLeaf(payload []byte) (Leaf, error) {
	corrupt := func(offset int, invariant string) error {
		return &CorruptionError{PageType: TypeBTreeLeaf, Offset: offset, Invariant: invariant}
	}
	if len(payload) != MaxPayloadSize {
		return Leaf{}, corrupt(0, fmt.Sprintf("leaf payload size is %d, expected %d", len(payload), MaxPayloadSize))
	}
	if payload[0] != leafFormatVersion {
		return Leaf{}, corrupt(0, fmt.Sprintf("unsupported leaf format version %d", payload[0]))
	}
	if payload[1] != 0 {
		return Leaf{}, corrupt(1, "leaf flags must be zero")
	}
	slotCount := int(binary.LittleEndian.Uint16(payload[2:4]))
	freeStart := int(binary.LittleEndian.Uint16(payload[4:6]))
	freeEnd := int(binary.LittleEndian.Uint16(payload[6:8]))
	expectedFreeStart := leafHeaderSize + slotCount*leafSlotSize
	if expectedFreeStart > len(payload) || freeStart != expectedFreeStart {
		return Leaf{}, corrupt(4, "free-space start does not follow slot array")
	}
	if freeEnd < freeStart || freeEnd > len(payload) {
		return Leaf{}, corrupt(6, "free-space end is outside payload")
	}
	for offset, value := range payload[freeStart:freeEnd] {
		if value != 0 {
			return Leaf{}, corrupt(freeStart+offset, "free space must be zero")
		}
	}

	type interval struct{ start, end int }
	intervals := make([]interval, slotCount)
	entries := make([]LeafEntry, slotCount)
	for index := range slotCount {
		slotOffset := leafHeaderSize + index*leafSlotSize
		recordOffset := int(binary.LittleEndian.Uint16(payload[slotOffset : slotOffset+2]))
		recordLength := int(binary.LittleEndian.Uint16(payload[slotOffset+2 : slotOffset+4]))
		if binary.LittleEndian.Uint16(payload[slotOffset+4:slotOffset+6]) != 0 || binary.LittleEndian.Uint16(payload[slotOffset+6:slotOffset+8]) != 0 {
			return Leaf{}, corrupt(slotOffset+4, "slot flags and reserved bytes must be zero")
		}
		if recordLength == 0 || recordOffset < freeEnd || recordOffset > len(payload)-recordLength {
			return Leaf{}, corrupt(slotOffset, "slot record range is outside packed record area")
		}
		record := payload[recordOffset : recordOffset+recordLength]
		keyLength, count := binary.Uvarint(record)
		if count <= 0 {
			return Leaf{}, corrupt(recordOffset, "record has invalid key-length varint")
		}
		var canonical [binary.MaxVarintLen64]byte
		canonicalLength := binary.PutUvarint(canonical[:], keyLength)
		if canonicalLength != count || !bytes.Equal(record[:count], canonical[:canonicalLength]) {
			return Leaf{}, corrupt(recordOffset, "record key-length varint is not canonical")
		}
		if keyLength > uint64(len(record)-count) {
			return Leaf{}, corrupt(recordOffset, "record key length exceeds record")
		}
		keyEnd := count + int(keyLength)
		entries[index] = LeafEntry{Key: bytes.Clone(record[count:keyEnd]), Value: bytes.Clone(record[keyEnd:])}
		intervals[index] = interval{start: recordOffset, end: recordOffset + recordLength}
	}
	sort.Slice(intervals, func(left, right int) bool { return intervals[left].start < intervals[right].start })
	expectedOffset := freeEnd
	for _, interval := range intervals {
		if interval.start != expectedOffset {
			return Leaf{}, corrupt(interval.start, "packed records overlap or contain gaps")
		}
		expectedOffset = interval.end
	}
	if expectedOffset != len(payload) {
		return Leaf{}, corrupt(expectedOffset, "packed records do not fill record area")
	}
	return Leaf{RightSibling: ID(binary.LittleEndian.Uint64(payload[8:16])), Entries: entries}, nil
}

func (b *LeafBuilder) encodedSize() int {
	size := leafHeaderSize + len(b.entries)*leafSlotSize
	for _, entry := range b.entries {
		size += uvarintLength(uint64(len(entry.Key))) + len(entry.Key) + len(entry.Value)
	}
	return size
}

func uvarintLength(value uint64) int {
	var buffer [binary.MaxVarintLen64]byte
	return binary.PutUvarint(buffer[:], value)
}

func cloneLeafEntry(entry LeafEntry) LeafEntry {
	return LeafEntry{Key: bytes.Clone(entry.Key), Value: bytes.Clone(entry.Value)}
}
