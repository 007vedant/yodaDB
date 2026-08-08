# ADR 0003: Use CRC-64/ECMA page checksums

- Status: Accepted
- Date: 2026-08-05
- Owners: storage

## Context

Every persisted page needs deterministic corruption detection with a standard,
inspectable implementation. The conceptual page header reserves 64 bits.

## Decision

Pages use CRC-64 with the ECMA polynomial, implemented with Go's `hash/crc64`
package. The checksum covers the encoded header with its checksum field set to
zero followed by exactly the declared payload bytes. Unused page bytes are not
covered and must have format-defined handling before M2.3.

Checksums detect accidental corruption; they are not authentication and provide
no protection against deliberate modification.

## Consequences

The implementation has no external dependency and diagnostics can reproduce a
checksum easily. M0 includes a repeatable checksum benchmark; M2 must add golden
vectors and single-byte corruption tests. Changing the algorithm requires a
format version and a superseding ADR.

## Alternatives considered

- CRC-32C was rejected because the planned header has a 64-bit checksum field.
- A cryptographic hash was rejected because adversarial tamper resistance is not
  a v1 requirement.
- A nonstandard fast hash was rejected in favor of a standard library algorithm
  with simple cross-platform behavior.
