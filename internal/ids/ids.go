// Package ids generates prefixed ULID identifiers used by control-plane entities.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"sync/atomic"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	Alias      = "als"
	Project    = "prj"
	Version    = "ver"
	Binding    = "bnd"
	Deployment = "dep"
	AuthPolicy = "pol"
)

var fallbackCounter uint64

// New returns a "<prefix>_<ULID>" identifier.
func New(prefix string) string {
	return prefix + "_" + ULID()
}

// ULID returns a 26-character Crockford base32 ULID.
func ULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		fillFallback(b[6:], ms)
	}

	var out [26]byte
	t := ms
	for i := 9; i >= 0; i-- {
		out[i] = alphabet[t&0x1f]
		t >>= 5
	}
	for i := 0; i < 16; i++ {
		var v byte
		for bit := 0; bit < 5; bit++ {
			pos := i*5 + bit
			if b[6+pos/8]&(1<<uint(7-pos%8)) != 0 {
				v |= 1 << uint(4-bit)
			}
		}
		out[10+i] = alphabet[v]
	}
	return string(out[:])
}

// fillFallback keeps identifiers unique when the system entropy source fails.
func fillFallback(dst []byte, seed uint64) {
	n := atomic.AddUint64(&fallbackCounter, 1)
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], seed)
	binary.BigEndian.PutUint64(buf[8:], n)
	for i := range dst {
		dst[i] = buf[i%len(buf)]
	}
}
