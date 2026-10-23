// Package btree reads immutable B+ tree roots, provides lookup, seek, and range
// cursors, and prepares copy-on-write leaf replacements. Parent/root mutation
// is added incrementally during M3.
package btree
