// Package sealed is the sealed request/reply exchange two services put on a TCP
// link: a challenge from the server, then one AEAD frame each way under a key
// both ends hold.
//
// It was internal/control's, and moved here unchanged the first time a second
// service needed it. internal/probe's health link is that service. Sharing the
// code rather than approximating it is the point: an exchange that differs in
// its framing is a different exchange to audit.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/olgm/proxy/internal/tunnel"
)

const (
	ChallengeLen = 16
	nonceLen     = 12
	// maxFrame bounds one sealed message. A list of a thousand players is under
	// 100 KB; anything near this is not a request.
	maxFrame = 1 << 20

	DirRequest byte = 0
	DirReply   byte = 1
)

var errShort = errors.New("sealed: frame shorter than its own framing")

// sealer is one key's AEAD. The tunnel numbers its nonces because a link carries
// millions of datagrams; a control link carries a few messages a day, so a random
// nonce is unique with room to spare. What makes a recorded message worthless
// later is the server's challenge in the associated data: it is fresh for every
// connection, so a frame sealed for one exchange opens in no other.
// Sealer is one key's AEAD.
type Sealer struct{ aead cipher.AEAD }

// NewSealer prepares a sealer.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != tunnel.KeyLen {
		return nil, errors.New("sealed: key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead}, nil
}

// Seal seals a message under the given associated data.
func (s *Sealer) Seal(plain, aad []byte) []byte {
	nonce := make([]byte, nonceLen, nonceLen+len(plain)+s.aead.Overhead())
	rand.Read(nonce) // cannot fail since Go 1.24
	return s.aead.Seal(nonce, nonce, plain, aad)
}

// Open reverses Seal, and fails on anything sealed for another exchange.
func (s *Sealer) Open(wire, aad []byte) ([]byte, error) {
	if len(wire) < nonceLen+s.aead.Overhead() {
		return nil, errShort
	}
	return s.aead.Open(nil, wire[:nonceLen], wire[nonceLen:], aad)
}

// aad binds a frame to one exchange and one direction, so a reply cannot be
// played back as a request or the other way round.
// AAD binds a frame to one exchange and one direction.
func AAD(challenge []byte, dir byte) []byte {
	return append(append(make([]byte, 0, ChallengeLen+1), challenge...), dir)
}

// WriteFrame writes one length-prefixed frame.
func WriteFrame(w io.Writer, b []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	_, err := w.Write(append(n[:], b...))
	return err
}

// ReadFrame reads one, refusing a length no request has.
func ReadFrame(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > maxFrame {
		return nil, fmt.Errorf("sealed: frame of %d bytes", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}
