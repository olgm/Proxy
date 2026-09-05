package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/olgm/proxy/internal/whitelist"
)

const dialTimeout = 10 * time.Second

// Client is one end of a control link: an address and the key that entry holds.
type Client struct {
	Addr string
	Key  []byte
}

// Do sends one request and returns the reply. The error covers the link only —
// unreachable, wrong key, malformed; a request the node refused comes back as a
// Reply with OK false and the reason in Error.
func (c *Client) Do(req Request) (Reply, error) {
	s, err := newSealer(c.Key)
	if err != nil {
		return Reply{}, err
	}
	conn, err := net.DialTimeout("tcp", c.Addr, dialTimeout)
	if err != nil {
		return Reply{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(exchangeTimeout))

	chal := make([]byte, challengeLen)
	if _, err := io.ReadFull(conn, chal); err != nil {
		return Reply{}, fmt.Errorf("control: %s: no challenge: %w", c.Addr, err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	if err := writeFrame(conn, s.seal(b, aad(chal, dirRequest))); err != nil {
		return Reply{}, fmt.Errorf("control: %s: %w", c.Addr, err)
	}
	wire, err := readFrame(conn)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		// The node closes without a word on a frame it cannot open.
		return Reply{}, fmt.Errorf("control: %s: no reply; wrong key?", c.Addr)
	}
	if err != nil {
		return Reply{}, fmt.Errorf("control: %s: %w", c.Addr, err)
	}
	plain, err := s.open(wire, aad(chal, dirReply))
	if err != nil {
		return Reply{}, fmt.Errorf("control: %s: reply does not open: %w", c.Addr, err)
	}
	var rep Reply
	if err := json.Unmarshal(plain, &rep); err != nil {
		return Reply{}, fmt.Errorf("control: %s: %w", c.Addr, err)
	}
	return rep, nil
}

// List returns every entry on the node.
func (c *Client) List() ([]whitelist.Entry, error) {
	rep, err := c.Do(Request{Op: "list"})
	if err != nil {
		return nil, err
	}
	if err := rep.Err(); err != nil {
		return nil, err
	}
	return rep.Entries, nil
}

// Add lists a player. Give a name, a uuid, or both.
func (c *Client) Add(name, uuid, tag string) (Reply, error) {
	return c.Do(Request{Op: "add", Name: name, UUID: uuid, Tag: tag})
}

// Remove drops a player by uuid.
func (c *Client) Remove(uuid string) (Reply, error) {
	return c.Do(Request{Op: "remove", UUID: uuid})
}
