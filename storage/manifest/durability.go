package manifest

import (
	"fmt"

	"yodadb/dberrors"
)

// Durability defines what a successful commit is synchronized against.
type Durability uint8

const (
	// DurabilityFull synchronizes data, the manifest, and publication metadata.
	// It is the zero value and the default.
	DurabilityFull Durability = iota
	// DurabilityProcess preserves ordered process-visible writes but omits sync
	// barriers, so it does not promise survival of power or kernel failure.
	DurabilityProcess
)

func (durability Durability) String() string {
	switch durability {
	case DurabilityFull:
		return "full"
	case DurabilityProcess:
		return "process"
	default:
		return fmt.Sprintf("durability(%d)", durability)
	}
}

func validateDurability(durability Durability, operation string) error {
	if durability != DurabilityFull && durability != DurabilityProcess {
		return dberrors.New(dberrors.CodeConstraint, operation, fmt.Sprintf("invalid durability mode %d", durability))
	}
	return nil
}
