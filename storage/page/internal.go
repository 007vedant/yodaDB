package page

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"yodadb/dberrors"
)

const (
	internalFormatVersion byte = 1
	internalHeaderSize         = 16
	internalSlotSize           = 16
	minimumTreePageID     ID   = 2
)

// InternalEntry routes keys greater than or equal to Key to RightChild.
type InternalEntry struct {
	Key        []byte
	RightChild ID
}

// Internal is a decoded B+ tree internal-page payload.
type Internal struct {
	FirstChild ID
	Entries    []InternalEntry
}

// InternalBuilder owns a mutable, uncommitted internal-page representation.
type InternalBuilder struct {
	firstChild ID
	entries    []InternalEntry
}

// NewInternalBuilder creates an empty internal-page builder.
func NewInternalBuilder(firstChild ID) *InternalBuilder {
	return &InternalBuilder{firstChild: firstChild}
}

// NewInternalBuilderFromDecoded creates a mutable copy of an internal page.
func NewInternalBuilderFromDecoded(internal Internal) *InternalBuilder {
	builder := NewInternalBuilder(internal.FirstChild)
	for _, entry := range internal.Entries {
		builder.entries = append(builder.entries, cloneInternalEntry(entry))
	}
	return builder
}

// Count returns the number of separator slots.
func (b *InternalBuilder) Count() int { return len(b.entries) }

// FirstChild returns the leftmost child page ID.
func (b *InternalBuilder) FirstChild() ID { return b.firstChild }

// SetFirstChild updates the builder's uncommitted leftmost child.
func (b *InternalBuilder) SetFirstChild(id ID) { b.firstChild = id }

// Entry returns a defensive copy of one separator entry.
func (b *InternalBuilder) Entry(index int) (InternalEntry, error) {
	if index < 0 || index >= len(b.entries) {
		return InternalEntry{}, dberrors.New(dberrors.CodeConstraint, "internal.entry", fmt.Sprintf("slot %d outside [0,%d)", index, len(b.entries)))
	}
	return cloneInternalEntry(b.entries[index]), nil
}

// Insert adds a separator and its right child at a logical slot position.
func (b *InternalBuilder) Insert(index int, key []byte, rightChild ID) error {
	if index < 0 || index > len(b.entries) {
		return dberrors.New(dberrors.CodeConstraint, "internal.insert", fmt.Sprintf("slot %d outside [0,%d]", index, len(b.entries)))
	}
	if len(key) == 0 {
		return dberrors.New(dberrors.CodeConstraint, "internal.insert", "separator key cannot be empty")
	}
	if rightChild < minimumTreePageID {
		return dberrors.New(dberrors.CodeConstraint, "internal.insert", fmt.Sprintf("child page %d is reserved", rightChild))
	}
	newSize := b.encodedSize() + internalSlotSize + len(key)
	if newSize > MaxPayloadSize {
		return dberrors.New(dberrors.CodeResourceLimit, "internal.insert", fmt.Sprintf("entry would require %d-byte payload, maximum is %d", newSize, MaxPayloadSize))
	}
	entry := InternalEntry{Key: bytes.Clone(key), RightChild: rightChild}
	b.entries = append(b.entries, InternalEntry{})
	copy(b.entries[index+1:], b.entries[index:])
	b.entries[index] = entry
	return nil
}

// Delete removes one separator slot from the uncommitted builder.
func (b *InternalBuilder) Delete(index int) error {
	if index < 0 || index >= len(b.entries) {
		return dberrors.New(dberrors.CodeConstraint, "internal.delete", fmt.Sprintf("slot %d outside [0,%d)", index, len(b.entries)))
	}
	copy(b.entries[index:], b.entries[index+1:])
	b.entries[len(b.entries)-1] = InternalEntry{}
	b.entries = b.entries[:len(b.entries)-1]
	return nil
}

// Encode returns the canonical full-size version-1 internal-page payload.
func (b *InternalBuilder) Encode() ([]byte, error) {
	if b.firstChild < minimumTreePageID {
		return nil, dberrors.New(dberrors.CodeConstraint, "internal.encode", fmt.Sprintf("first child page %d is reserved", b.firstChild))
	}
	if len(b.entries) == 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "internal.encode", "internal page requires at least one separator")
	}
	if b.encodedSize() > MaxPayloadSize {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "internal.encode", "internal page exceeds page payload capacity")
	}
	for index, entry := range b.entries {
		if len(entry.Key) == 0 || entry.RightChild < minimumTreePageID {
			return nil, dberrors.New(dberrors.CodeConstraint, "internal.encode", fmt.Sprintf("invalid separator %d", index))
		}
		if index > 0 && bytes.Compare(b.entries[index-1].Key, entry.Key) >= 0 {
			return nil, dberrors.New(dberrors.CodeConstraint, "internal.encode", "separator keys must be strictly increasing")
		}
	}

	payload := make([]byte, MaxPayloadSize)
	payload[0] = internalFormatVersion
	binary.LittleEndian.PutUint16(payload[2:4], uint16(len(b.entries)))
	freeStart := internalHeaderSize + len(b.entries)*internalSlotSize
	freeEnd := len(payload)
	binary.LittleEndian.PutUint16(payload[4:6], uint16(freeStart))
	binary.LittleEndian.PutUint64(payload[8:16], uint64(b.firstChild))
	for index, entry := range b.entries {
		freeEnd -= len(entry.Key)
		copy(payload[freeEnd:], entry.Key)
		slotOffset := internalHeaderSize + index*internalSlotSize
		binary.LittleEndian.PutUint16(payload[slotOffset:slotOffset+2], uint16(freeEnd))
		binary.LittleEndian.PutUint16(payload[slotOffset+2:slotOffset+4], uint16(len(entry.Key)))
		binary.LittleEndian.PutUint64(payload[slotOffset+8:slotOffset+16], uint64(entry.RightChild))
	}
	binary.LittleEndian.PutUint16(payload[6:8], uint16(freeEnd))
	return payload, nil
}

// DecodeInternal validates and decodes a canonical version-1 internal payload.
func DecodeInternal(payload []byte) (Internal, error) {
	corrupt := func(offset int, invariant string) error {
		return &CorruptionError{PageType: TypeBTreeInternal, Offset: offset, Invariant: invariant}
	}
	if len(payload) != MaxPayloadSize {
		return Internal{}, corrupt(0, fmt.Sprintf("internal payload size is %d, expected %d", len(payload), MaxPayloadSize))
	}
	if payload[0] != internalFormatVersion {
		return Internal{}, corrupt(0, fmt.Sprintf("unsupported internal format version %d", payload[0]))
	}
	if payload[1] != 0 {
		return Internal{}, corrupt(1, "internal flags must be zero")
	}
	slotCount := int(binary.LittleEndian.Uint16(payload[2:4]))
	if slotCount == 0 {
		return Internal{}, corrupt(2, "internal page requires at least one separator")
	}
	freeStart := int(binary.LittleEndian.Uint16(payload[4:6]))
	freeEnd := int(binary.LittleEndian.Uint16(payload[6:8]))
	expectedFreeStart := internalHeaderSize + slotCount*internalSlotSize
	if expectedFreeStart > len(payload) || freeStart != expectedFreeStart {
		return Internal{}, corrupt(4, "free-space start does not follow slot array")
	}
	if freeEnd < freeStart || freeEnd > len(payload) {
		return Internal{}, corrupt(6, "free-space end is outside payload")
	}
	firstChild := ID(binary.LittleEndian.Uint64(payload[8:16]))
	if firstChild < minimumTreePageID {
		return Internal{}, corrupt(8, fmt.Sprintf("first child page %d is reserved", firstChild))
	}
	for offset, value := range payload[freeStart:freeEnd] {
		if value != 0 {
			return Internal{}, corrupt(freeStart+offset, "free space must be zero")
		}
	}

	type interval struct{ start, end int }
	intervals := make([]interval, slotCount)
	entries := make([]InternalEntry, slotCount)
	for index := range slotCount {
		slotOffset := internalHeaderSize + index*internalSlotSize
		keyOffset := int(binary.LittleEndian.Uint16(payload[slotOffset : slotOffset+2]))
		keyLength := int(binary.LittleEndian.Uint16(payload[slotOffset+2 : slotOffset+4]))
		if binary.LittleEndian.Uint16(payload[slotOffset+4:slotOffset+6]) != 0 || binary.LittleEndian.Uint16(payload[slotOffset+6:slotOffset+8]) != 0 {
			return Internal{}, corrupt(slotOffset+4, "slot flags and reserved bytes must be zero")
		}
		rightChild := ID(binary.LittleEndian.Uint64(payload[slotOffset+8 : slotOffset+16]))
		if rightChild < minimumTreePageID {
			return Internal{}, corrupt(slotOffset+8, fmt.Sprintf("right child page %d is reserved", rightChild))
		}
		if keyLength == 0 || keyOffset < freeEnd || keyOffset > len(payload)-keyLength {
			return Internal{}, corrupt(slotOffset, "separator range is outside packed key area")
		}
		key := bytes.Clone(payload[keyOffset : keyOffset+keyLength])
		if index > 0 && bytes.Compare(entries[index-1].Key, key) >= 0 {
			return Internal{}, corrupt(keyOffset, "separator keys must be strictly increasing")
		}
		entries[index] = InternalEntry{Key: key, RightChild: rightChild}
		intervals[index] = interval{start: keyOffset, end: keyOffset + keyLength}
	}
	sort.Slice(intervals, func(left, right int) bool { return intervals[left].start < intervals[right].start })
	expectedOffset := freeEnd
	for _, interval := range intervals {
		if interval.start != expectedOffset {
			return Internal{}, corrupt(interval.start, "packed separator keys overlap or contain gaps")
		}
		expectedOffset = interval.end
	}
	if expectedOffset != len(payload) {
		return Internal{}, corrupt(expectedOffset, "packed separator keys do not fill key area")
	}
	return Internal{FirstChild: firstChild, Entries: entries}, nil
}

func (b *InternalBuilder) encodedSize() int {
	size := internalHeaderSize + len(b.entries)*internalSlotSize
	for _, entry := range b.entries {
		size += len(entry.Key)
	}
	return size
}

func cloneInternalEntry(entry InternalEntry) InternalEntry {
	return InternalEntry{Key: bytes.Clone(entry.Key), RightChild: entry.RightChild}
}
