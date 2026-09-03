package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// KeyLen is the length of a link key. 32 bytes selects AES-256-GCM.
const KeyLen = 32

var (
	errReplay  = errors.New("tunnel: replayed datagram")
	errBadKey  = errors.New("tunnel: key must be 32 bytes")
	errTooShrt = errors.New("tunnel: datagram shorter than its own framing")
)

// NewKey returns a fresh link key.
func NewKey() []byte {
	k := make([]byte, KeyLen)
	if _, err := rand.Read(k); err != nil {
		panic("tunnel: no entropy: " + err.Error())
	}
	return k
}

// EncodeKey and DecodeKey move a key through JSON config.
func EncodeKey(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func DecodeKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("tunnel: key: %w", err)
	}
	if len(k) != KeyLen {
		return nil, errBadKey
	}
	return k, nil
}

// sealer authenticates one link. Both ends hold the same key, and without it a
// datagram is not merely unreadable but unforgeable — which is the whole reason
// this exists. Over TCP a source-address allowlist was enough, because an address
// has to complete a handshake before it can use one. Over UDP it is not: anyone
// who can spoof the previous hop's address could otherwise inject bytes into a
// live session, or make a relay reflect traffic on their behalf.
type sealer struct {
	aead  cipher.AEAD
	epoch uint32 // ours, random per process, so counters never repeat across restarts
	ctr   atomic.Uint64

	mu   sync.Mutex
	seen replay
}

func newSealer(key []byte) (*sealer, error) {
	if len(key) != KeyLen {
		return nil, errBadKey
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	var e [4]byte
	if _, err := rand.Read(e[:]); err != nil {
		return nil, err
	}
	return &sealer{aead: aead, epoch: binary.BigEndian.Uint32(e[:])}, nil
}

// nonceLen is epoch + counter. Counters are never reused under one epoch, so the
// nonce is unique by construction rather than by luck, which a random nonce this
// short could not promise over a long-lived link.
const nonceLen = 4 + 8

// seal produces one datagram. Every copy of a duplicated chunk is sealed
// separately: identical bytes on the wire would be indistinguishable from a replay
// and the receiver would drop the second copy, defeating the point.
func (s *sealer) seal(dst, plain []byte) []byte {
	var nonce [nonceLen]byte
	binary.BigEndian.PutUint32(nonce[:], s.epoch)
	binary.BigEndian.PutUint64(nonce[4:], s.ctr.Add(1))
	dst = append(dst, nonce[:]...)
	return s.aead.Seal(dst, nonce[:], plain, nil)
}

func (s *sealer) open(dst, wire []byte) ([]byte, error) {
	if len(wire) < nonceLen+s.aead.Overhead() {
		return nil, errTooShrt
	}
	nonce := wire[:nonceLen]
	out, err := s.aead.Open(dst, nonce, wire[nonceLen:], nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	ok := s.seen.accept(binary.BigEndian.Uint32(nonce), binary.BigEndian.Uint64(nonce[4:]))
	s.mu.Unlock()
	if !ok {
		return nil, errReplay
	}
	return out, nil
}

// replay is a sliding window over the peer's nonce counters. The stream layer
// already ignores a chunk it has seen, so this is not what stops a duplicate from
// being delivered twice; it stops a captured datagram from being *re-injected*
// later, which for a stream-opening chunk would mean one more TCP connection to
// the backend from our egress address for every copy an attacker kept.
type replay struct {
	epoch uint32
	set   bool
	high  uint64
	bits  [16]uint64 // 1024 counters
}

const replayWindow = 64 * len(replay{}.bits)

func (r *replay) accept(epoch uint32, ctr uint64) bool {
	if !r.set || epoch != r.epoch {
		// Only a holder of the key can present a new epoch, so this is a restart
		// of the peer, not an attack: start the window again.
		*r = replay{epoch: epoch, set: true, high: ctr}
		r.mark(0)
		return true
	}
	switch {
	case ctr > r.high:
		r.shift(ctr - r.high)
		r.high = ctr
		r.mark(0)
		return true
	case r.high-ctr >= uint64(replayWindow):
		return false
	default:
		i := int(r.high - ctr)
		if r.test(i) {
			return false
		}
		r.mark(i)
		return true
	}
}

func (r *replay) mark(i int)      { r.bits[i/64] |= 1 << (i % 64) }
func (r *replay) test(i int) bool { return r.bits[i/64]&(1<<(i%64)) != 0 }

func (r *replay) shift(n uint64) {
	if n >= uint64(replayWindow) {
		r.bits = [16]uint64{}
		return
	}
	words, bits := int(n/64), uint(n%64)
	for i := len(r.bits) - 1; i >= 0; i-- {
		var v uint64
		if i-words >= 0 {
			v = r.bits[i-words] << bits
			if bits > 0 && i-words-1 >= 0 {
				v |= r.bits[i-words-1] >> (64 - bits)
			}
		}
		r.bits[i] = v
	}
}
