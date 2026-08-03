package thermal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"math"
)

// fingerprintWriter is the cross-language content-addressing format used by
// HomeSpec v2 and thermal artifacts v2. Each value has an explicit type and
// fixed-width length or representation, so Unicode, '<', '&', -0, and float
// formatting cannot make Go and Python hash different bytes.
type fingerprintWriter struct {
	h hash.Hash
}

func newFingerprint(domain string) *fingerprintWriter {
	w := &fingerprintWriter{h: sha256.New()}
	w.String(domain)
	return w
}

func (w *fingerprintWriter) writeUint64(value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	_, _ = w.h.Write(data[:])
}

func (w *fingerprintWriter) String(value string) {
	_, _ = w.h.Write([]byte{'s'})
	data := []byte(value)
	w.writeUint64(uint64(len(data)))
	_, _ = w.h.Write(data)
}

func (w *fingerprintWriter) Float(value float64) {
	_, _ = w.h.Write([]byte{'f'})
	w.writeUint64(math.Float64bits(value))
}

func (w *fingerprintWriter) Int(value int) {
	_, _ = w.h.Write([]byte{'i'})
	w.writeUint64(uint64(int64(value)))
}

func (w *fingerprintWriter) Bool(value bool) {
	_, _ = w.h.Write([]byte{'b'})
	if value {
		_, _ = w.h.Write([]byte{1})
	} else {
		_, _ = w.h.Write([]byte{0})
	}
}

func (w *fingerprintWriter) OptionalFloat(value *float64) {
	_, _ = w.h.Write([]byte{'o'})
	if value == nil {
		_, _ = w.h.Write([]byte{0})
		return
	}
	_, _ = w.h.Write([]byte{1})
	w.Float(*value)
}

func (w *fingerprintWriter) StringList(values []string) {
	_, _ = w.h.Write([]byte{'l'})
	w.writeUint64(uint64(len(values)))
	for _, value := range values {
		w.String(value)
	}
}

func (w *fingerprintWriter) Sum() string {
	return hex.EncodeToString(w.h.Sum(nil))
}
