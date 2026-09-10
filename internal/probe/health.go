package probe

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/olgm/proxy/internal/sealed"
	"github.com/olgm/proxy/internal/tunnel"
)

// The health link exists for one question the bot cannot answer any other way:
// is probed running? A node that is down cannot say so, and proxyd's own control
// link says nothing about probed — the two services do not know about each other
// and this does not change that. It is a separate port under a separate key, and
// everything it can tell you is already public within our own infrastructure.
const healthTimeout = 10 * time.Second

// Health is the whole answer. It carries enough to tell "running" from "running
// but measuring nothing", which is a real state: a config with no classes starts
// cleanly and reports nothing forever.
type Health struct {
	Node    string `json:"node"`
	Classes int    `json:"classes"`
	UpSecs  int64  `json:"up_seconds"`
	// Legs is every class this node originates, with its newest short-window
	// measurement. It is on the health answer because there is no other way for
	// the bot to reach a number: the measurements are written to probed's log on
	// the node, and a status card needs one latency per node without the dataset
	// being shipped anywhere. A node that only answers probes reports none.
	Legs []Leg `json:"legs,omitempty"`
}

// Leg is one class's newest closed window. P50, N and AgeSecs are all negative
// when no window has closed yet: a class that has measured nothing reports
// nothing, and a zero would read as instant.
type Leg struct {
	Class  string  `json:"class"`
	Kind   string  `json:"kind"`
	Window string  `json:"window,omitempty"`
	P50    float64 `json:"p50"`
	// Loss is the round-trip figure as a percentage, the same one the feed
	// reports.
	Loss float64 `json:"loss"`
	N    int     `json:"n"`
	// AgeSecs is how long ago that window closed. A figure without its age
	// cannot be told apart from a leg that stopped being measured an hour ago,
	// which is exactly the case a status card has to get right.
	AgeSecs int64 `json:"age_seconds"`
}

// HealthConfig is the link, when one is deployed. Absent means probed listens for
// nothing but probes, which is what a topology with no status feed gets.
type HealthConfig struct {
	Bind string `json:"bind"`
	// Key seals the exchange. It is probed's own and is not the node's control
	// key: holding it must not be a way into a session, which is the same reason
	// the probe keys are not proxyd's.
	Key string `json:"key"`
	// AllowFrom is who may connect besides loopback: the bot's node. TCP, so an
	// address means something here in a way it never does over UDP.
	AllowFrom []string `json:"allow_from,omitempty"`
}

type healthServer struct {
	seal    *sealed.Sealer
	allow   []netip.Prefix
	node    *Node
	started time.Time
}

// serveHealth starts the link. It is the node's, not a class's: what it reports
// is about the process.
func (n *Node) serveHealth(c *HealthConfig) (net.Listener, error) {
	key, err := tunnel.DecodeKey(c.Key)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	s, err := sealed.NewSealer(key)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	var allow []netip.Prefix
	for _, a := range c.AllowFrom {
		p, err := parsePrefix(a)
		if err != nil {
			return nil, fmt.Errorf("health: allow_from %q: %w", a, err)
		}
		allow = append(allow, p)
	}
	ln, err := net.Listen("tcp", c.Bind)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	h := &healthServer{seal: s, allow: allow, node: n, started: time.Now()}
	go h.serve(ln)
	log.Printf("%s: health %s", n.cfg.Name, c.Bind)
	return ln, nil
}

func (h *healthServer) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("health: accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go h.handle(c)
	}
}

func (h *healthServer) handle(c net.Conn) {
	defer c.Close()
	if !h.allowed(c.RemoteAddr()) {
		log.Printf("health: reject %s", c.RemoteAddr())
		return
	}
	c.SetDeadline(time.Now().Add(healthTimeout))

	chal := make([]byte, sealed.ChallengeLen)
	rand.Read(chal)
	if _, err := c.Write(chal); err != nil {
		return
	}
	wire, err := sealed.ReadFrame(c)
	if err != nil {
		return
	}
	// A frame that does not open gets nothing back, for the reason the control
	// link gives nothing back: a refusal tells a guesser something is here.
	if _, err := h.seal.Open(wire, sealed.AAD(chal, sealed.DirRequest)); err != nil {
		log.Printf("health: %s: refused: %v", c.RemoteAddr(), err)
		return
	}
	b, err := json.Marshal(Health{
		Node:    h.node.cfg.Name,
		Classes: len(h.node.classes),
		UpSecs:  int64(time.Since(h.started).Seconds()),
		Legs:    h.node.Legs(),
	})
	if err != nil {
		return
	}
	sealed.WriteFrame(c, h.seal.Seal(b, sealed.AAD(chal, sealed.DirReply)))
}

func (h *healthServer) allowed(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range h.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// parsePrefix takes a bare address as well as a CIDR, so allow_from can name a
// node by its address the way the rest of the topology does.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// HealthClient is the bot's end. The request body is empty on purpose: there is
// only one question, and the challenge is what makes asking it unforgeable.
type HealthClient struct {
	Addr string
	Key  []byte
}

func (c *HealthClient) Check() (Health, error) {
	s, err := sealed.NewSealer(c.Key)
	if err != nil {
		return Health{}, err
	}
	conn, err := net.DialTimeout("tcp", c.Addr, healthTimeout)
	if err != nil {
		return Health{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(healthTimeout))

	chal := make([]byte, sealed.ChallengeLen)
	if _, err := io.ReadFull(conn, chal); err != nil {
		return Health{}, fmt.Errorf("health: %s: no challenge: %w", c.Addr, err)
	}
	if err := sealed.WriteFrame(conn, s.Seal([]byte("{}"), sealed.AAD(chal, sealed.DirRequest))); err != nil {
		return Health{}, err
	}
	wire, err := sealed.ReadFrame(conn)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return Health{}, fmt.Errorf("health: %s: no reply; wrong key?", c.Addr)
	}
	if err != nil {
		return Health{}, err
	}
	plain, err := s.Open(wire, sealed.AAD(chal, sealed.DirReply))
	if err != nil {
		return Health{}, fmt.Errorf("health: %s: reply does not open: %w", c.Addr, err)
	}
	var h Health
	if err := json.Unmarshal(plain, &h); err != nil {
		return Health{}, err
	}
	return h, nil
}
