package control

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
	challengeLen = 16
	nonceLen     = 12
	// maxFrame bounds one sealed message. A list of a thousand players is under
	// 100 KB; anything near this is not a request.
	maxFrame = 1 << 20

	dirRequest byte = 0
	dirReply   byte = 1
)

var errShort = errors.New("control: frame shorter than its own framing")

// sealer is one key's AEAD. The tunnel numbers its nonces because a link carries
// millions of datagrams; a control link carries a few messages a day, so a random
// nonce is unique with room to spare. What makes a recorded message worthless
// later is the server's challenge in the associated data: it is fresh for every
// connection, so a frame sealed for one exchange opens in no other.
type sealer struct{ aead cipher.AEAD }

func newSealer(key []byte) (*sealer, error) {
	if len(key) != tunnel.KeyLen {
		return nil, errors.New("control: key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &sealer{aead}, nil
}

func (s *sealer) seal(plain, aad []byte) []byte {
	nonce := make([]byte, nonceLen, nonceLen+len(plain)+s.aead.Overhead())
	rand.Read(nonce) // cannot fail since Go 1.24
	return s.aead.Seal(nonce, nonce, plain, aad)
}

func (s *sealer) open(wire, aad []byte) ([]byte, error) {
	if len(wire) < nonceLen+s.aead.Overhead() {
		return nil, errShort
	}
	return s.aead.Open(nil, wire[:nonceLen], wire[nonceLen:], aad)
}

// aad binds a frame to one exchange and one direction, so a reply cannot be
// played back as a request or the other way round.
func aad(challenge []byte, dir byte) []byte {
	return append(append(make([]byte, 0, challengeLen+1), challenge...), dir)
}

func writeFrame(w io.Writer, b []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	_, err := w.Write(append(n[:], b...))
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > maxFrame {
		return nil, fmt.Errorf("control: frame of %d bytes", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}
