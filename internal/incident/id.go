package incident

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewID returns a fresh ULID string. ULIDs sort lexicographically by
// generation time, which makes them pleasant in logs and as
// debug-visible identifiers. The randomness source is crypto/rand.
func NewID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}
