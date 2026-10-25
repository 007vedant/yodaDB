package txn

import (
	"bytes"
	"fmt"
	"sort"
	"unicode/utf8"

	"yodadb/dberrors"
)

// ResourceKind identifies the logical namespace used for conflict detection.
type ResourceKind uint8

const (
	// RowResource identifies one table row by its encoded table key.
	RowResource ResourceKind = iota + 1
	// UniqueKeyResource identifies one key in a unique secondary index.
	UniqueKeyResource
	// CatalogResource identifies a catalog object or name.
	CatalogResource
)

// CatalogKind identifies a catalog namespace whose IDs and names are unique.
type CatalogKind uint8

const (
	CatalogTable CatalogKind = iota + 1
	CatalogIndex
	CatalogSnapshot
)

// Resource is an immutable logical conflict identity. Key bytes are stored as a
// string so Resource remains comparable and can safely be used as a map key.
type Resource struct {
	kind        ResourceKind
	scopeID     uint64
	catalogKind CatalogKind
	key         string
}

// NewRowResource identifies one row in a table. encodedKey must be the exact
// canonical key used by the table tree.
func NewRowResource(tableID uint64, encodedKey []byte) (Resource, error) {
	return newKeyResource(RowResource, tableID, encodedKey, "table")
}

// NewUniqueKeyResource identifies one logical key in a unique index.
func NewUniqueKeyResource(indexID uint64, encodedKey []byte) (Resource, error) {
	return newKeyResource(UniqueKeyResource, indexID, encodedKey, "index")
}

// NewCatalogObjectResource identifies an existing catalog object by stable ID.
func NewCatalogObjectResource(kind CatalogKind, objectID uint64) (Resource, error) {
	if err := validateCatalogKind(kind); err != nil {
		return Resource{}, err
	}
	if objectID == 0 {
		return Resource{}, dberrors.New(dberrors.CodeConstraint, "txn.resource", "catalog object ID must be nonzero")
	}
	return Resource{kind: CatalogResource, scopeID: objectID, catalogKind: kind}, nil
}

// NewCatalogNameResource identifies a name in a catalog namespace. scopeID is
// the owning table ID for index names and zero for table and snapshot names. It
// allows concurrent creates and renames to conflict before a stable ID exists.
func NewCatalogNameResource(kind CatalogKind, scopeID uint64, name string) (Resource, error) {
	if err := validateCatalogKind(kind); err != nil {
		return Resource{}, err
	}
	if kind == CatalogIndex && scopeID == 0 {
		return Resource{}, dberrors.New(dberrors.CodeConstraint, "txn.resource", "index-name scope must be a nonzero table ID")
	}
	if kind != CatalogIndex && scopeID != 0 {
		return Resource{}, dberrors.New(dberrors.CodeConstraint, "txn.resource", "table and snapshot names must use global scope")
	}
	if name == "" || !utf8.ValidString(name) {
		return Resource{}, dberrors.New(dberrors.CodeConstraint, "txn.resource", "catalog name must be non-empty UTF-8")
	}
	return Resource{kind: CatalogResource, scopeID: scopeID, catalogKind: kind, key: name}, nil
}

// Kind returns the resource namespace.
func (r Resource) Kind() ResourceKind { return r.kind }

// ScopeID returns the table ID, index ID, catalog object ID, or the owning table
// ID of an index name. Globally scoped catalog names return zero.
func (r Resource) ScopeID() uint64 { return r.scopeID }

// CatalogKind returns the catalog namespace, or zero for row and unique-key
// resources.
func (r Resource) CatalogKind() CatalogKind { return r.catalogKind }

// Key returns a defensive copy of the encoded row/index key or catalog name.
func (r Resource) Key() []byte { return bytes.Clone([]byte(r.key)) }

// Footprint records the logical resources read and written by one transaction.
// Its zero value is ready for use.
type Footprint struct {
	reads  map[Resource]struct{}
	writes map[Resource]struct{}
}

// RecordRead adds a logical resource to the read set.
func (f *Footprint) RecordRead(resource Resource) error {
	if err := validateResource(resource); err != nil {
		return err
	}
	if f.reads == nil {
		f.reads = make(map[Resource]struct{})
	}
	f.reads[resource] = struct{}{}
	return nil
}

// RecordWrite adds a logical resource to the write set.
func (f *Footprint) RecordWrite(resource Resource) error {
	if err := validateResource(resource); err != nil {
		return err
	}
	if f.writes == nil {
		f.writes = make(map[Resource]struct{})
	}
	f.writes[resource] = struct{}{}
	return nil
}

// Reads returns a deterministic snapshot of the read set.
func (f *Footprint) Reads() []Resource {
	if f == nil {
		return nil
	}
	return sortedResources(f.reads)
}

// Writes returns a deterministic snapshot of the write set.
func (f *Footprint) Writes() []Resource {
	if f == nil {
		return nil
	}
	return sortedResources(f.writes)
}

// WriteConflicts returns the resources written by both footprints. Reads do
// not conflict under yodaDB's snapshot-isolation target.
func (f *Footprint) WriteConflicts(other *Footprint) []Resource {
	if f == nil || other == nil {
		return nil
	}
	conflicts := make(map[Resource]struct{})
	for resource := range f.writes {
		if _, exists := other.writes[resource]; exists {
			conflicts[resource] = struct{}{}
		}
	}
	return sortedResources(conflicts)
}

func newKeyResource(kind ResourceKind, scopeID uint64, encodedKey []byte, scope string) (Resource, error) {
	if scopeID == 0 {
		return Resource{}, dberrors.New(dberrors.CodeConstraint, "txn.resource", scope+" ID must be nonzero")
	}
	if len(encodedKey) == 0 {
		return Resource{}, dberrors.New(dberrors.CodeConstraint, "txn.resource", "encoded key must not be empty")
	}
	return Resource{kind: kind, scopeID: scopeID, key: string(encodedKey)}, nil
}

func validateResource(resource Resource) error {
	switch resource.kind {
	case RowResource, UniqueKeyResource:
		if resource.scopeID == 0 || resource.key == "" || resource.catalogKind != 0 {
			return dberrors.New(dberrors.CodeConstraint, "txn.footprint", "invalid row or unique-key resource")
		}
	case CatalogResource:
		if err := validateCatalogKind(resource.catalogKind); err != nil {
			return err
		}
		if resource.key == "" {
			if resource.scopeID == 0 {
				return dberrors.New(dberrors.CodeConstraint, "txn.footprint", "catalog object ID must be nonzero")
			}
		} else if resource.catalogKind == CatalogIndex {
			if resource.scopeID == 0 {
				return dberrors.New(dberrors.CodeConstraint, "txn.footprint", "index-name scope must be a nonzero table ID")
			}
		} else if resource.scopeID != 0 {
			return dberrors.New(dberrors.CodeConstraint, "txn.footprint", "table and snapshot names must use global scope")
		}
	default:
		return dberrors.New(dberrors.CodeConstraint, "txn.footprint", "unknown resource kind")
	}
	return nil
}

func validateCatalogKind(kind CatalogKind) error {
	switch kind {
	case CatalogTable, CatalogIndex, CatalogSnapshot:
		return nil
	default:
		return dberrors.New(dberrors.CodeConstraint, "txn.resource", fmt.Sprintf("unknown catalog kind %d", kind))
	}
}

func sortedResources(set map[Resource]struct{}) []Resource {
	resources := make([]Resource, 0, len(set))
	for resource := range set {
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(left, right int) bool {
		a, b := resources[left], resources[right]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.catalogKind != b.catalogKind {
			return a.catalogKind < b.catalogKind
		}
		if a.scopeID != b.scopeID {
			return a.scopeID < b.scopeID
		}
		return a.key < b.key
	})
	return resources
}
