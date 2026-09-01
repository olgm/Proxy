// Package proxy is the node runtime. A node has no role: it is a list of
// listeners, each forwarding to one upstream. A listener with a Minecraft block
// is an ingress, one without is a relay. That is the only difference between the
// three machines in a chain.
package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/olgm/proxy/internal/mc"
	"github.com/olgm/proxy/internal/mojang"
	"github.com/olgm/proxy/internal/whitelist"
)

const (
	// handshakeTimeout bounds how long a client may take to send its handshake,
	// and its Login Start when there is a whitelist to check it against. Cleared
	// once we start relaying: the Play stream is long-lived and idles between
	// Hypixel's ~15s keepalives.
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 10 * time.Second

	// denyMessage is what a non-whitelisted player sees. Dropping the connection
	// instead would be indistinguishable from the chain being down.
	denyMessage = "You are not whitelisted on this proxy."
)

type Config struct {
	Listeners []Listener `json:"listeners"`
}

type Listener struct {
	Bind     string `json:"bind"`
	Upstream string `json:"upstream"`
	// AllowFrom is a list of source IPs or CIDRs. Empty means allow anyone, which
	// is only correct for a public ingress: a relay left open is a free proxy to
	// the backend, and the abuse lands on our egress IP.
	AllowFrom []string   `json:"allow_from,omitempty"`
	Minecraft *Minecraft `json:"minecraft,omitempty"`
}

type Minecraft struct {
	RewriteHost string `json:"rewrite_host"`
	RewritePort uint16 `json:"rewrite_port"`
	// Whitelist is the path to an ign:uuid list. Empty means anyone may log in.
	// Only an ingress can hold one: it is the only hop that sees a Login Start.
	Whitelist string `json:"whitelist,omitempty"`
}

type server struct {
	Listener
	allow []netip.Prefix
	wl    *whitelist.List
}

// newMojang builds the profile-API client backing the whitelist. A seam: tests
// replace it so they neither reach the network nor depend on Mojang being up.
var newMojang = func() whitelist.Mojang { return mojang.New() }

// Run starts every listener and blocks.
func Run(cfg *Config) error {
	if len(cfg.Listeners) == 0 {
		return errors.New("config defines no listeners")
	}
	var wg sync.WaitGroup
	for _, l := range cfg.Listeners {
		s, err := newServer(l)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", l.Bind)
		if err != nil {
			return err
		}
		mode := "relay"
		if l.Minecraft != nil {
			mode = "minecraft->" + l.Minecraft.RewriteHost
		}
		allow := "any"
		if len(l.AllowFrom) > 0 {
			allow = strings.Join(l.AllowFrom, ",")
		}
		if s.wl != nil {
			mode += fmt.Sprintf(" whitelist=%s(%d)", l.Minecraft.Whitelist, s.wl.Len())
			// Keeps the name column fresh enough that a released name stops
			// matching here long before anyone else can claim it.
			s.wl.StartRefresh()
		}
		log.Printf("listen %s -> %s [%s] allow=%s", l.Bind, l.Upstream, mode, allow)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.accept(ln)
		}()
	}
	wg.Wait()
	return nil
}

func newServer(l Listener) (*server, error) {
	if l.Bind == "" || l.Upstream == "" {
		return nil, fmt.Errorf("listener %q: bind and upstream are both required", l.Bind)
	}
	s := &server{Listener: l}
	for _, a := range l.AllowFrom {
		p, err := parsePrefix(a)
		if err != nil {
			return nil, fmt.Errorf("listener %s: allow_from %q: %w", l.Bind, a, err)
		}
		s.allow = append(s.allow, p)
	}
	if l.Minecraft != nil && l.Minecraft.Whitelist != "" {
		// Refusing to start beats starting ungated: an unreadable list would
		// otherwise silently open the chain to everyone.
		wl, err := whitelist.Open(l.Minecraft.Whitelist, newMojang())
		if err != nil {
			return nil, fmt.Errorf("listener %s: whitelist: %w", l.Bind, err)
		}
		s.wl = wl
	}
	return s, nil
}

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

func (s *server) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept errors (fd exhaustion) shouldn't spin the CPU.
			log.Printf("%s: accept: %v", s.Bind, err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handle(c.(*net.TCPConn))
	}
}

func (s *server) handle(c *net.TCPConn) {
	defer c.Close()
	c.SetNoDelay(true)

	if !s.allowed(c.RemoteAddr()) {
		log.Printf("%s: reject %s", s.Bind, c.RemoteAddr())
		return
	}
	if s.Minecraft != nil {
		s.handleMinecraft(c)
		return
	}
	u, err := s.dial()
	if err != nil {
		log.Printf("%s: dial %s: %v", s.Bind, s.Upstream, err)
		return
	}
	defer u.Close()
	relay(c, u)
}

func (s *server) handleMinecraft(c *net.TCPConn) {
	c.SetReadDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReaderSize(c, 4096)
	h, err := mc.ReadHandshake(br)
	if err != nil {
		log.Printf("%s: handshake from %s: %v", s.Bind, c.RemoteAddr(), err)
		return
	}

	// The whitelist check has to happen before we dial, so a stranger costs the
	// chain nothing and never reaches Hypixel from our egress IP. Status pings
	// carry no identity and are left alone: gating them would hide the MOTD from
	// whitelisted players without keeping anyone out.
	var login []byte
	if s.wl != nil && h.Intent != mc.IntentStatus {
		ls, raw, err := mc.ReadLoginStart(br, h.ProtocolVersion)
		if err != nil {
			log.Printf("%s: login start from %s: %v", s.Bind, c.RemoteAddr(), err)
			return
		}
		if !s.wl.Check(ls.Name, ls.UUIDString(), sourceIP(c.RemoteAddr())) {
			log.Printf("%s: deny %s name=%q uuid=%q proto=%d",
				s.Bind, c.RemoteAddr(), ls.Name, ls.UUIDString(), h.ProtocolVersion)
			c.Write(mc.EncodeLoginDisconnect(denyMessage))
			return
		}
		login = raw
	}
	c.SetReadDeadline(time.Time{})

	h.RewriteAddress(s.Minecraft.RewriteHost)
	if s.Minecraft.RewritePort != 0 {
		h.Port = s.Minecraft.RewritePort
	}

	u, err := s.dial()
	if err != nil {
		log.Printf("%s: dial %s: %v", s.Bind, s.Upstream, err)
		return
	}
	defer u.Close()

	if _, err := u.Write(h.Encode()); err != nil {
		return
	}
	// A Login Start we read to check the whitelist goes back on the wire verbatim,
	// and ahead of anything still buffered behind it.
	if login != nil {
		if _, err := u.Write(login); err != nil {
			return
		}
	}
	// The client may have pipelined Login Start (or a status request) behind the
	// handshake; bufio has already pulled those bytes off the socket, so hand them
	// over before dropping to raw relay.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := u.Write(b); err != nil {
			return
		}
	}
	relay(c, u)
}

func (s *server) dial() (*net.TCPConn, error) {
	c, err := net.DialTimeout("tcp", s.Upstream, dialTimeout)
	if err != nil {
		return nil, err
	}
	t := c.(*net.TCPConn)
	t.SetNoDelay(true)
	return t, nil
}

// sourceIP is the rate-limiting key for Mojang lookups: the one thing a client
// cannot forge over TCP.
func sourceIP(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.IP.String()
	}
	return a.String()
}

func (s *server) allowed(a net.Addr) bool {
	if len(s.allow) == 0 {
		return true
	}
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, p := range s.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// relay copies in both directions until both are done. Each direction half-closes
// its own side on EOF rather than tearing down the whole connection, so a client
// that stops sending doesn't cut off data still in flight from the server.
//
// Both ends are *net.TCPConn, so io.Copy uses splice(2) on Linux and the payload
// never enters userspace.
func relay(a, b *net.TCPConn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(a, b, &wg)
	go pipe(b, a, &wg)
	wg.Wait()
}

func pipe(dst, src *net.TCPConn, wg *sync.WaitGroup) {
	defer wg.Done()
	io.Copy(dst, src)
	dst.CloseWrite()
}
