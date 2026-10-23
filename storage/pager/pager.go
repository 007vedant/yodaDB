package pager

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"yodadb/dberrors"
	"yodadb/storage/page"
	"yodadb/storage/vfs"
)

const defaultCachePages = 64

// Stats describes current cache usage and cumulative lookup outcomes.
type Stats struct {
	Entries int
	Pinned  int
	Hits    uint64
	Misses  uint64
}

type cacheEntry struct {
	page     page.Page
	pins     int
	lastUsed uint64
}

// Pager validates fixed-size pages and never allocates an existing page ID.
type Pager struct {
	mu         sync.Mutex
	file       vfs.File
	pageCount  uint64
	cacheLimit int
	cache      map[page.ID]*cacheEntry
	clock      uint64
	hits       uint64
	misses     uint64
	closed     bool
}

// Open creates a pager over an existing page-aligned file. A non-positive cache
// limit selects a conservative default.
func Open(file vfs.File, cachePages int) (*Pager, error) {
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size < 0 || size%page.Size != 0 {
		return nil, dberrors.New(dberrors.CodeCorruption, "pager.open", fmt.Sprintf("file size %d is not a multiple of page size %d", size, page.Size))
	}
	if cachePages <= 0 {
		cachePages = defaultCachePages
	}
	return &Pager{
		file:       file,
		pageCount:  uint64(size / page.Size),
		cacheLimit: cachePages,
		cache:      make(map[page.ID]*cacheEntry),
	}, nil
}

// Read returns a pinned, validated page handle. Callers must Release it.
func (p *Pager) Read(id page.ID) (*Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "pager.read", "pager is closed")
	}
	if uint64(id) >= p.pageCount {
		return nil, dberrors.New(dberrors.CodeConstraint, "pager.read", fmt.Sprintf("page %d outside file page count %d", id, p.pageCount))
	}
	p.clock++
	if entry, ok := p.cache[id]; ok {
		entry.pins++
		entry.lastUsed = p.clock
		p.hits++
		return &Handle{pager: p, id: id, page: entry.page}, nil
	}
	if len(p.cache) >= p.cacheLimit && !p.evictOneLocked() {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "pager.read", "all cache entries are pinned")
	}
	p.misses++
	buffer := make([]byte, page.Size)
	offset, err := pageOffset(id)
	if err != nil {
		return nil, err
	}
	if err := readFullAt(p.file, buffer, offset); err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeIO, "pager.read", fmt.Sprintf("page %d", id))
	}
	decoded, err := page.Decode(buffer)
	if err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeCorruption, "pager.read", fmt.Sprintf("page %d", id))
	}
	if decoded.Header.PageID != id {
		return nil, dberrors.New(dberrors.CodeCorruption, "pager.read", fmt.Sprintf("page at position %d declares page ID %d", id, decoded.Header.PageID))
	}
	p.cache[id] = &cacheEntry{page: decoded, pins: 1, lastUsed: p.clock}
	return &Handle{pager: p, id: id, page: decoded}, nil
}

// Append writes a new page at the end of the file without synchronizing it.
// The caller controls the commit's synchronization boundary.
func (p *Pager) Append(pageType page.Type, generation uint64, payload []byte) (page.ID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, dberrors.New(dberrors.CodeTransactionClosed, "pager.append", "pager is closed")
	}
	id := page.ID(p.pageCount)
	if id < 2 && pageType != page.TypeMeta {
		return 0, dberrors.New(dberrors.CodeConstraint, "pager.append", fmt.Sprintf("reserved page %d must be a metapage", id))
	}
	if id >= 2 && pageType == page.TypeMeta {
		return 0, dberrors.New(dberrors.CodeConstraint, "pager.append", "metapages are restricted to pages 0 and 1")
	}
	encoded, err := page.Encode(page.Header{Type: pageType, PageID: id, Generation: generation}, payload)
	if err != nil {
		return 0, err
	}
	offset, err := pageOffset(id)
	if err != nil {
		return 0, err
	}
	if err := writeFullAt(p.file, encoded, offset); err != nil {
		_ = p.file.Truncate(offset)
		return 0, dberrors.Wrap(err, dberrors.CodeIO, "pager.append", fmt.Sprintf("page %d", id))
	}
	p.pageCount++
	return id, nil
}

// Sync synchronizes appended pages according to the VFS implementation.
func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return dberrors.New(dberrors.CodeTransactionClosed, "pager.sync", "pager is closed")
	}
	return p.file.Sync()
}

// PageCount returns the number of complete pages known to the pager.
func (p *Pager) PageCount() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pageCount
}

// Stats returns a consistent cache snapshot.
func (p *Pager) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := Stats{Entries: len(p.cache), Hits: p.hits, Misses: p.misses}
	for _, entry := range p.cache {
		stats.Pinned += entry.pins
	}
	return stats
}

// Close closes the underlying file if no page handles remain pinned.
func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	for _, entry := range p.cache {
		if entry.pins > 0 {
			return dberrors.New(dberrors.CodeConstraint, "pager.close", "cannot close pager with pinned pages")
		}
	}
	p.closed = true
	return p.file.Close()
}

func (p *Pager) evictOneLocked() bool {
	var candidate page.ID
	var oldest uint64
	found := false
	for id, entry := range p.cache {
		if entry.pins != 0 {
			continue
		}
		if !found || entry.lastUsed < oldest {
			candidate, oldest, found = id, entry.lastUsed, true
		}
	}
	if found {
		delete(p.cache, candidate)
	}
	return found
}

// Handle owns one cache pin.
type Handle struct {
	mu       sync.Mutex
	pager    *Pager
	id       page.ID
	page     page.Page
	released bool
}

// Page returns a defensive copy of the validated page.
func (h *Handle) Page() page.Page {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := h.page
	result.Payload = append([]byte(nil), h.page.Payload...)
	return result
}

// Release drops the cache pin and is idempotent.
func (h *Handle) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || h.pager == nil {
		return
	}
	h.pager.mu.Lock()
	if entry, ok := h.pager.cache[h.id]; ok && entry.pins > 0 {
		entry.pins--
	}
	h.pager.mu.Unlock()
	h.released = true
}

func pageOffset(id page.ID) (int64, error) {
	if uint64(id) > uint64(math.MaxInt64/page.Size) {
		return 0, dberrors.New(dberrors.CodeResourceLimit, "pager.offset", fmt.Sprintf("page ID %d overflows file offset", id))
	}
	return int64(id) * page.Size, nil
}

func readFullAt(file vfs.File, buffer []byte, offset int64) error {
	read := 0
	for read < len(buffer) {
		count, err := file.ReadAt(buffer[read:], offset+int64(read))
		read += count
		if read == len(buffer) {
			return nil
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func writeFullAt(file vfs.File, buffer []byte, offset int64) error {
	written := 0
	for written < len(buffer) {
		count, err := file.WriteAt(buffer[written:], offset+int64(written))
		written += count
		if written == len(buffer) {
			return nil
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// IsEOF reports whether an error chain contains an end-of-file condition. It is
// useful to fault-injection VFS implementations that preserve wrapped causes.
func IsEOF(err error) bool { return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) }
