package auth

import "github.com/google/uuid"

// NewID mints a UUIDv7 (time-sortable) and returns its canonical
// string form. Used as the primary key for every row Sajni creates.
// v7 keeps insert locality so B-tree indexes don't shuffle on every
// write, AND avoids the enumeration risk of monotonic integer ids.
func NewID() string {
	return uuid.Must(uuid.NewV7()).String()
}
