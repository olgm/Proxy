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
	"sync/atomic"
	"time"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/mc"
	"github.com/olgm/proxy/internal/mojang"
	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/whitelist"
)

const (
	// handshakeTimeout bounds how long a client may take to send its handshake,
	// and its Login Start when there is a whitelist to check it against. Cleared
	// once we start relaying: the Play stream is long-lived and idles between
	// Hypixel's ~15s keepalives.
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 10 * time.Second

	// maxChainDelay caps how long the ingress will hold a pong to report the
	// chain's latency. A chain that has stopped answering leaves a stale figure
	// behind; past this the server list should look bad, not hang.
	maxChainDelay = 2 * time.Second

	// denyMessage is what a non-whitelisted player sees. Dropping the connection
	// instead would be indistinguishable from the chain being down.
	denyMessage = "You are not whitelisted on this proxy."
)

type Config struct {
	Listeners []Listener `json:"listeners"`
	// Control is the link through which this node's whitelist is managed from
	// outside. Only a node with a whitelist has one.
	Control *Control `json:"control,omitempty"`
}

// Control is a TCP listener answering whitelist requests from the Discord bot,
// wherever it runs, and from `proxyd ctl` over ssh. See internal/control.
type Control struct {
	Bind string `json:"bind"`
	// Key seals every frame; the bot's node holds the same one.
	Key string `json:"key"`
	// AllowFrom is who may connect besides loopback, which always may: the bot's
	// node. TCP, so trusting an address is sound here, and the key is on top of
	// it rather than instead.
	AllowFrom []string `json:"allow_from,omitempty"`
}

type Listener struct {
	// Net is what this listener accepts on: "tcp" (default) or "udp". Players
	// always arrive over TCP, so the entry of a UDP tunnel is still a TCP
	// listener — it is the hop after it that changes.
	Net  string `json:"net,omitempty"`
	Bind string `json:"bind"`
	// Upstream is a TCP address dialled once per connection or stream. It is the
	// next hop on a plain TCP chain, and the backend itself at the exit of a
	// tunnel.
	Upstream string `json:"upstream,omitempty"`
	// Hops are the next nodes toward the exit, over UDP. Setting them replaces
	// Upstream: this listener hands the stream to the tunnel instead of dialling.
	// More than one is a race — every hop gets every chunk and the exit keeps
	// whichever copy arrives first.
	Hops []Link `json:"hops,omitempty"`
	// Peers are the previous nodes, over UDP. Required on a UDP listener.
	Peers []Link `json:"peers,omitempty"`
	// AllowFrom is a list of source IPs or CIDRs. Empty means allow anyone, which
	// is only correct for a public ingress: a relay left open is a free proxy to
	// the backend, and the abuse lands on our egress IP. It applies to TCP only —
	// over UDP a source address proves nothing, and the link key does this job.
	AllowFrom []string   `json:"allow_from,omitempty"`
	Minecraft *Minecraft `json:"minecraft,omitempty"`
	Tunnel    *Tunnel    `json:"tunnel,omitempty"`
}

// Link is one leg of a tunnel, from this node's point of view.
type Link struct {
	// Addr is host:port for a hop we dial, and a bare IP for a peer that dials us:
	// a node that dials uses an ephemeral source port, so there is none to match.
	Addr string `json:"addr"`
	// Key is a base64 32-byte key, unique per leg. Over UDP anyone can put the
	// previous hop's address on a datagram, so this — not allow_from — is what
	// keeps a relay from being an open reflector and a session from being injected
	// into.
	Key string `json:"key"`
	// Duplicate is how many copies of each chunk this node puts on this leg. It is
	// per leg and per node: a relay drops every copy but the first of what
	// arrives, then sends on with the count set for the leg after, so a lossy leg
	// can carry more than a clean one without the counts compounding. Default 1.
	Duplicate int `json:"duplicate,omitempty"`
}

// Tunnel is optional tuning. Everything here has a working default; the fields
// exist because the right value depends on the path, not on Minecraft.
type Tunnel struct {
	// MaxDatagram bounds a datagram before IP and UDP headers. Lower it if a leg
	// runs inside another tunnel and 1200 no longer fits.
	MaxDatagram int `json:"max_datagram,omitempty"`
	// Window is how many unacknowledged bytes one direction of one stream may hold
	// before the sender stops reading from the socket behind it.
	Window int `json:"window,omitempty"`
	// RepairMS is how long a hole may go unfilled before the stream is declared
	// unrecoverable and the session ends.
	RepairMS int `json:"repair_ms,omitempty"`
	// IdleMS reaps a relay's stream state after this long without a datagram.
	IdleMS int `json:"idle_ms,omitempty"`

	// Timers, all in milliseconds, zero for the default. Minecraft traffic is
	// light, so these can be far more aggressive than the defaults without the
	// extra packets mattering; the defaults lean toward not wasting a leg.
	TickMS int `json:"tick_ms,omitempty"` // granularity of every timer below; 5
	PingMS int `json:"ping_ms,omitempty"` // per link; down after five unanswered; 1000
	// NackMinMS and NackMaxMS clamp the re-ask interval for a hole, and the
	// repeat of a quiet sender's horizon: srtt + 4·mdev on that leg, held
	// between these. Equal values fix it. 10 and 1000.
	NackMinMS int `json:"nack_min_ms,omitempty"`
	NackMaxMS int `json:"nack_max_ms,omitempty"`
	// HeadQuietMS is how long a sender with chunks unacknowledged stays silent
	// before telling the next hop how far it got. 10.
	HeadQuietMS int `json:"head_quiet_ms,omitempty"`
	// AckMS bounds how often the exit reports what it has read when that has
	// moved; AckRepeatMS re-announces it when it has not. 20 and 250.
	AckMS       int `json:"ack_ms,omitempty"`
	AckRepeatMS int `json:"ack_repeat_ms,omitempty"`
	// ProbeMinMS and ProbeMaxMS clamp the entry's blind re-send of its highest
	// chunk, the backstop behind the horizon: end-to-end srtt + 4·mdev, doubling
	// on each try. 100 and 1000.
	ProbeMinMS int `json:"probe_min_ms,omitempty"`
	ProbeMaxMS int `json:"probe_max_ms,omitempty"`
}

type Minecraft struct {
	RewriteHost string `json:"rewrite_host"`
	RewritePort uint16 `json:"rewrite_port"`
	// Whitelist is the path to an ign:uuid list. Empty means anyone may log in.
	// Only an ingress can hold one: it is the only hop that sees a Login Start.
	Whitelist string `json:"whitelist,omitempty"`
	// Motd is the path to the JSON document this ingress answers a server-list
	// ping with. Empty uses the built-in one. There is no setting for forwarding
	// the backend's own: see serveStatus.
	Motd string `json:"motd,omitempty"`
}

type server struct {
	Listener
	allow  []netip.Prefix
	wl     *whitelist.List
	mojang mojangAPI // set with wl; the control link resolves names through it
	tun    *tunnel.Node
	motd   *mc.Motd
	// online is the count of logins currently being relayed, which is the number
	// the MOTD reports and the only live state this node keeps about a session.
	online atomic.Int64
}

// halfCloser is whatever the next leg turns out to be: a TCP connection on a
// plain chain, a tunnel stream on a UDP one. Both are byte pipes that can end one
// direction without cutting off the other, which is what lets a client stop
// talking while the server is still sending.
type halfCloser interface {
	io.ReadWriter
	CloseWrite() error
	Close() error
}

// mojangAPI is everything the node asks Mojang: the whitelist's refresh and
// miss-path lookup, and the control link's name resolution.
type mojangAPI interface {
	whitelist.Mojang
	control.Resolver
}

// newMojang builds the profile-API client backing the whitelist. A seam: tests
// replace it so they neither reach the network nor depend on Mojang being up.
var newMojang = func() mojangAPI { return mojang.New() }

// Run starts every listener and blocks.
func Run(cfg *Config) error {
	n, err := start(cfg)
	if err != nil {
		return err
	}
	n.wg.Wait()
	return nil
}

// node is a running config: its listeners, and the control link if it has one.
type node struct {
	wg      sync.WaitGroup
	lns     []net.Listener
	servers []*server
	ctl     net.Listener
}

func (n *node) close() {
	for _, ln := range n.lns {
		ln.Close()
	}
	for _, s := range n.servers {
		if s.tun != nil {
			s.tun.Close()
		}
	}
	if n.ctl != nil {
		n.ctl.Close()
	}
}

func start(cfg *Config) (*node, error) {
	if len(cfg.Listeners) == 0 {
		return nil, errors.New("config defines no listeners")
	}
	if cfg.Control != nil && !gated(cfg) {
		return nil, errors.New("control: this node has no whitelist to manage")
	}
	n := &node{}
	var wg = &n.wg
	for _, l := range cfg.Listeners {
		s, err := newServer(l)
		if err != nil {
			return nil, err
		}
		n.servers = append(n.servers, s)
		mode := l.Role()
		if s.wl != nil {
			mode += fmt.Sprintf(" whitelist=%s(%d)", l.Minecraft.Whitelist, s.wl.Len())
			// Keeps the name column fresh enough that a released name stops
			// matching here long before anyone else can claim it.
			s.wl.StartRefresh()
		}
		guard := "allow=any"
		if len(l.AllowFrom) > 0 {
			guard = "allow=" + strings.Join(l.AllowFrom, ",")
		}
		if l.Net == "udp" {
			guard = "peers=" + strings.Join(l.PeerAddrs(), ",")
		}
		log.Printf("listen %s %s -> %s [%s] %s", l.Network(), l.Bind, l.Next(), mode, guard)

		wg.Add(1)
		if l.Net == "udp" {
			// The tunnel node is the listener: it bound its own socket when it was
			// built, and hands us streams instead of connections.
			go func() {
				defer wg.Done()
				s.serveTunnel()
			}()
			continue
		}
		ln, err := net.Listen("tcp", l.Bind)
		if err != nil {
			return nil, err
		}
		n.lns = append(n.lns, ln)
		go func() {
			defer wg.Done()
			s.accept(ln)
		}()
	}
	if cfg.Control != nil {
		for _, s := range n.servers {
			if s.wl == nil {
				continue
			}
			ln, err := startControl(cfg.Control, s)
			if err != nil {
				return nil, err
			}
			n.ctl = ln
			break
		}
	}
	return n, nil
}

// gated reports whether any listener holds a whitelist.
func gated(cfg *Config) bool {
	for _, l := range cfg.Listeners {
		if l.Minecraft != nil && l.Minecraft.Whitelist != "" {
			return true
		}
	}
	return false
}

// startControl serves the control link over the first whitelisted listener's
// list. A node has at most one whitelist file, so any further whitelisted
// listener sees the same edits on its next reload.
func startControl(c *Control, s *server) (net.Listener, error) {
	key, err := tunnel.DecodeKey(c.Key)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	var allow []netip.Prefix
	for _, a := range c.AllowFrom {
		p, err := parsePrefix(a)
		if err != nil {
			return nil, fmt.Errorf("control: allow_from %q: %w", a, err)
		}
		allow = append(allow, p)
	}
	srv, err := control.NewServer(s.wl, key, allow, s.mojang)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", c.Bind)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	guard := "loopback"
	if len(c.AllowFrom) > 0 {
		guard += "," + strings.Join(c.AllowFrom, ",")
	}
	log.Printf("control %s whitelist=%s allow=%s", c.Bind, s.Minecraft.Whitelist, guard)
	go srv.Serve(ln)
	return ln, nil
}

// Role is what this listener does, which is not configured anywhere: it follows
// from which fields are set. A Minecraft block makes it an ingress, a UDP
// listener that dials the target rather than another hop is a tunnel's exit, and
// anything else passes bytes along.
func (l Listener) Role() string {
	switch {
	case l.Minecraft != nil:
		return "minecraft->" + l.Minecraft.RewriteHost
	case l.Net == "udp" && l.Upstream != "":
		return "exit"
	default:
		return "relay"
	}
}

// Network is what this listener accepts on, defaulted.
func (l Listener) Network() string {
	if l.Net == "udp" {
		return "udp"
	}
	return "tcp"
}

// Next describes where this listener sends: one TCP address, or every path of a
// tunnel with the copies each one carries. proxyctl prints the same thing, so a
// preview and a running node describe themselves the same way.
func (l Listener) Next() string {
	if len(l.Hops) == 0 {
		return l.Upstream
	}
	parts := make([]string, 0, len(l.Hops))
	for _, h := range l.Hops {
		p := h.Addr
		if h.Duplicate > 1 {
			p += fmt.Sprintf("x%d", h.Duplicate)
		}
		parts = append(parts, p)
	}
	return "udp:" + strings.Join(parts, "+")
}

// PeerAddrs are the addresses a UDP listener will answer, with the copies it
// sends back to each, for the same reason.
func (l Listener) PeerAddrs() []string {
	out := make([]string, 0, len(l.Peers))
	for _, p := range l.Peers {
		a := p.Addr
		if p.Duplicate > 1 {
			a += fmt.Sprintf("x%d", p.Duplicate)
		}
		out = append(out, a)
	}
	return out
}

func newServer(l Listener) (*server, error) {
	switch l.Net {
	case "", "tcp", "udp":
	default:
		return nil, fmt.Errorf("listener %s: net must be tcp or udp, not %q", l.Bind, l.Net)
	}
	if l.Bind == "" {
		return nil, fmt.Errorf("listener: bind is required")
	}
	if (l.Upstream == "") == (len(l.Hops) == 0) {
		return nil, fmt.Errorf("listener %s: set exactly one of upstream and hops", l.Bind)
	}
	if l.Net == "udp" && len(l.Peers) == 0 {
		return nil, fmt.Errorf("listener %s: a udp listener needs peers to authenticate", l.Bind)
	}
	if l.Net != "udp" && len(l.Peers) > 0 {
		return nil, fmt.Errorf("listener %s: peers only mean something on a udp listener", l.Bind)
	}
	if l.Net == "udp" && l.Minecraft != nil {
		return nil, fmt.Errorf("listener %s: a Minecraft block belongs on the tcp listener players reach", l.Bind)
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
		m := newMojang()
		wl, err := whitelist.Open(l.Minecraft.Whitelist, m)
		if err != nil {
			return nil, fmt.Errorf("listener %s: whitelist: %w", l.Bind, err)
		}
		s.wl, s.mojang = wl, m
	}
	if l.Minecraft != nil {
		// Loaded whether or not it is configured: an ingress always answers a
		// status ping itself, so it always needs something to answer with.
		m, err := mc.LoadMotd(l.Minecraft.Motd)
		if err != nil {
			return nil, fmt.Errorf("listener %s: %w", l.Bind, err)
		}
		s.motd = m
	}
	if len(l.Hops) > 0 || len(l.Peers) > 0 {
		t, err := newTunnel(l)
		if err != nil {
			return nil, err
		}
		s.tun = t
	}
	return s, nil
}

func newTunnel(l Listener) (*tunnel.Node, error) {
	opt := tunnel.Options{Name: l.Bind}
	if l.Net == "udp" {
		opt.Bind = l.Bind
	}
	var err error
	if opt.Peers, err = tunnelLinks(l.Peers); err != nil {
		return nil, fmt.Errorf("listener %s: peers: %w", l.Bind, err)
	}
	if opt.Hops, err = tunnelLinks(l.Hops); err != nil {
		return nil, fmt.Errorf("listener %s: hops: %w", l.Bind, err)
	}
	if t := l.Tunnel; t != nil {
		ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
		opt.MaxDatagram = t.MaxDatagram
		opt.Window = t.Window
		opt.Repair = ms(t.RepairMS)
		opt.Idle = ms(t.IdleMS)
		opt.Timers = tunnel.Timers{
			Tick: ms(t.TickMS), Ping: ms(t.PingMS),
			NackMin: ms(t.NackMinMS), NackMax: ms(t.NackMaxMS),
			HeadQuiet: ms(t.HeadQuietMS),
			AckEvery:  ms(t.AckMS), AckRepeat: ms(t.AckRepeatMS),
			ProbeMin: ms(t.ProbeMinMS), ProbeMax: ms(t.ProbeMaxMS),
		}
	}
	n, err := tunnel.New(opt)
	if err != nil {
		return nil, fmt.Errorf("listener %s: %w", l.Bind, err)
	}
	return n, nil
}

func tunnelLinks(in []Link) ([]tunnel.LinkConfig, error) {
	out := make([]tunnel.LinkConfig, 0, len(in))
	for _, l := range in {
		if l.Addr == "" {
			return nil, errors.New("addr is required")
		}
		k, err := tunnel.DecodeKey(l.Key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.Addr, err)
		}
		out = append(out, tunnel.LinkConfig{Addr: l.Addr, Key: k, Dup: l.Duplicate})
	}
	return out, nil
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
	u, err := s.connect()
	if err != nil {
		log.Printf("%s: open %s: %v", s.Bind, s.Next(), err)
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
	if h.Intent == mc.IntentStatus {
		s.serveStatus(c, br, h)
		return
	}
	s.serveLogin(c, br, h)
}

// serveStatus answers a server-list ping from this node and never dials.
//
// Forwarding one would put a status request at the backend for every refresh of
// every client's multiplayer screen, and for every scanner that finds 25565 open
// on a public ingress — all of it arriving from the egress address, which is the
// hardest thing in this system to replace. agents/operational-safety.md forbids
// probing the backend from a node; this is the path that would otherwise do it
// thousands of times a day without anyone running a command.
func (s *server) serveStatus(c *net.TCPConn, br *bufio.Reader, h *mc.Handshake) {
	for {
		c.SetReadDeadline(time.Now().Add(handshakeTimeout))
		p, err := mc.ReadStatusPacket(br)
		if err != nil {
			return
		}
		switch p.ID {
		case mc.PacketStatusRequest:
			doc := s.motd.Render(h.ProtocolVersion, int(s.online.Load()))
			if _, err := c.Write(mc.EncodeStatusResponse(doc)); err != nil {
				return
			}
		case mc.PacketPing:
			// Nothing in the status exchange reports a latency: the client times
			// the pong itself. Answering at once would show the distance to this
			// node, which is the near end of a chain the player's traffic has to
			// cross all of — so hold the answer for as long as the rest of the
			// chain takes, and the number lands where a player can act on it.
			if d := s.chainDelay(); d > 0 {
				time.Sleep(d)
			}
			c.Write(mc.EncodePong(p.Body))
			return
		default:
			return
		}
	}
}

// chainDelay is how much longer than this node the whole chain takes to answer.
// Zero on a plain TCP chain, which has nothing measuring itself, and zero until
// the first echo comes back. Capped so a chain that has gone away leaves a player
// waiting on the server list rather than waiting on us.
func (s *server) chainDelay() time.Duration {
	if s.tun == nil {
		return 0
	}
	return min(s.tun.ChainRTT(), maxChainDelay)
}

func (s *server) serveLogin(c *net.TCPConn, br *bufio.Reader, h *mc.Handshake) {
	ip := sourceIP(c.RemoteAddr())

	// The whitelist check has to happen before we dial, so a stranger costs the
	// chain nothing and never reaches the backend from our egress IP.
	var login []byte
	var name, uuid string
	if s.wl != nil {
		ls, raw, err := mc.ReadLoginStart(br, h.ProtocolVersion)
		if err != nil {
			log.Printf("%s: login start from %s: %v", s.Bind, c.RemoteAddr(), err)
			return
		}
		name, uuid = ls.Name, ls.UUIDString()
		if ls.Trailing {
			log.Printf("%s: %s claims protocol %d but did not send that packet; matching on name alone",
				s.Bind, ip, h.ProtocolVersion)
		}
		if !s.wl.Check(name, uuid, ip) {
			log.Printf("%s: deny %s name=%q uuid=%q proto=%d", s.Bind, ip, name, uuid, h.ProtocolVersion)
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

	u, err := s.connect()
	if err != nil {
		log.Printf("%s: open %s: %v", s.Bind, s.Next(), err)
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
	// The client may have pipelined Login Start behind the handshake; bufio has
	// already pulled those bytes off the socket, so hand them over before dropping
	// to raw relay.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := u.Write(b); err != nil {
			return
		}
	}

	start := time.Now()
	online := s.online.Add(1)
	log.Printf("%s: login %s name=%q uuid=%q proto=%d online=%d", s.Bind, ip, name, uuid, h.ProtocolVersion, online)

	up, down := relay(c, u)

	log.Printf("%s: logout %s name=%q uuid=%q for %s up=%s down=%s%s online=%d",
		s.Bind, ip, name, uuid, time.Since(start).Round(time.Second),
		size(up), size(down), wireCost(u, up+down), s.online.Add(-1))
}

// wireCost reports what the tunnel actually spent carrying a session, and how much
// more that was than the session itself. Duplication is set per leg, so the payload
// figure alone never shows what a chain costs to run — this is the number that says
// whether a duplicate count is worth what it is buying.
func wireCost(u halfCloser, payload int64) string {
	st, ok := u.(*tunnel.Stream)
	if !ok {
		return ""
	}
	wire := st.Wire()
	if wire == 0 || payload <= 0 {
		return fmt.Sprintf(" wire=%s", size(int64(wire)))
	}
	return fmt.Sprintf(" wire=%s(x%.2f)", size(int64(wire)), float64(wire)/float64(payload))
}

// size renders a byte count the way an operator reads one.
func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	v, exp := float64(n)/unit, 0
	for v >= unit && exp < 3 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", v, "KMGT"[exp])
}

// serveTunnel is the exit's accept loop. A relay never reaches the body: it has
// hops of its own, so no stream ever terminates on it.
func (s *server) serveTunnel() {
	for {
		st, err := s.tun.Accept()
		if err != nil {
			return
		}
		go s.serveStream(st)
	}
}

func (s *server) serveStream(st *tunnel.Stream) {
	defer st.Close()
	u, err := s.dial()
	if err != nil {
		log.Printf("%s: dial %s: %v", s.Bind, s.Upstream, err)
		return
	}
	defer u.Close()
	relay(st, u)
}

// connect opens the next leg: a TCP dial on a plain chain, a tunnel stream when
// the hop after this one runs over UDP.
func (s *server) connect() (halfCloser, error) {
	if len(s.Hops) > 0 {
		return s.tun.Open()
	}
	return s.dial()
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
// When both ends are *net.TCPConn this still reaches splice(2) on Linux and the
// payload never enters userspace: io.Copy looks at the concrete type, which the
// interface does not hide from it. A tunnel stream on one side gives that up,
// because the bytes have to be numbered before they can be sent.
// The byte counts it returns are payload: what the session carried, before the
// tunnel duplicated any of it.
func relay(a, b halfCloser) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(b, a, &aToB, &wg)
	go pipe(a, b, &bToA, &wg)
	wg.Wait()
	return aToB, bToA
}

func pipe(dst, src halfCloser, n *int64, wg *sync.WaitGroup) {
	defer wg.Done()
	*n, _ = io.Copy(dst, src)
	dst.CloseWrite()
}
