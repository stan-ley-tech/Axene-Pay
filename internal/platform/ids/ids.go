// Package ids generates and validates public resource identifiers.
//
// Format (decision D-11): a lower-case type prefix, an underscore, and 26
// characters of Crockford base32 encoding 128 random bits, e.g.
// "pay_01J9Z3K8Q4V7M2N5R6T8W0X1Y2". IDs are opaque to clients; the prefix
// exists only so humans and logs can tell resource types apart.
package ids

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

// Prefix identifies a resource type.
type Prefix string

const (
	Merchant              Prefix = "mer"
	SettlementDestination Prefix = "dst"
	ProviderConfig        Prefix = "pcf"
	APIKey                Prefix = "key"
	Payment               Prefix = "pay"
	Attempt               Prefix = "att"
	Callback              Prefix = "cbk"
	Event                 Prefix = "evt"
	ReconException        Prefix = "rex"
)

const (
	encoding   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32, no I L O U
	bodyLength = 26                                 // ceil(128 / 5)
)

// ErrMalformed is returned by Parse for strings that are not valid IDs of
// the expected type.
var ErrMalformed = errors.New("ids: malformed identifier")

// New returns a fresh random identifier with the given prefix.
func New(p Prefix) string {
	var b [16]byte
	// crypto/rand.Read never returns an error on supported platforms (Go 1.24+).
	_, _ = rand.Read(b[:])
	return string(p) + "_" + encode(b)
}

// Parse checks that s is a well-formed identifier of type p and returns it
// unchanged. Use it on every ID received from a client before querying.
func Parse(p Prefix, s string) (string, error) {
	body, ok := strings.CutPrefix(s, string(p)+"_")
	if !ok || len(body) != bodyLength {
		return "", fmt.Errorf("%w: expected %s_ followed by %d characters", ErrMalformed, p, bodyLength)
	}
	// The first character carries only the top 3 bits of 128 (26*5 = 130).
	if strings.IndexByte(encoding[:8], body[0]) < 0 {
		return "", fmt.Errorf("%w: out of range", ErrMalformed)
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(encoding, body[i]) < 0 {
			return "", fmt.Errorf("%w: invalid character", ErrMalformed)
		}
	}
	return s, nil
}

// encode renders 128 bits as 26 base32 characters, most significant first.
func encode(b [16]byte) string {
	var out [bodyLength]byte
	// Treat b as a 130-bit number with two leading zero bits.
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := bodyLength - 1; i >= 0; i-- {
		out[i] = encoding[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
