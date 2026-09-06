// Package control is the link through which a node's whitelist is managed from
// outside proxyd: by the Discord bot, from whichever node it runs on, and by
// proxyctl over ssh through `proxyd ctl`. It is TCP, one request per connection,
// every frame sealed with the entry's control key and bound to a challenge the
// server picked for that connection, so a recorded exchange cannot be replayed.
//
// proxyd stays the only writer of its whitelist file. Everything here goes through
// whitelist.List, which reloads before it writes, so an edit made on the node by
// hand is never undone by a request that arrived a moment later.
package control

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/olgm/proxy/internal/mojang"
	"github.com/olgm/proxy/internal/sealed"
	"github.com/olgm/proxy/internal/whitelist"
)

// exchangeTimeout bounds one connection end to end. An add may wait on Mojang for
// up to five seconds; nothing else here takes time.
const exchangeTimeout = 20 * time.Second

// Entry is one line of a whitelist, as the link carries it.
type Entry = whitelist.Entry

// Request is one operation. Op is list, add or remove. An add carries a name, a
// uuid, or both; the node resolves whichever is missing against Mojang. A remove
// carries a uuid: the client finds it from a list, so a name that two lines share
// never removes the wrong one.
type Request struct {
	Op   string `json:"op"`
	Name string `json:"name,omitempty"`
	UUID string `json:"uuid,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// Reply is the answer. Entry is the line an add wrote or a remove dropped; on an
// add refused because the UUID is already listed, Listed is set and Entry is the
// line it has, so the caller can say whose it is.
type Reply struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error,omitempty"`
	Listed  bool              `json:"listed,omitempty"`
	Entry   *whitelist.Entry  `json:"entry,omitempty"`
	Entries []whitelist.Entry `json:"entries,omitempty"`
	// Live answers the sessions op: who this node is relaying right now.
	Live []Live `json:"live,omitempty"`
}

// Live is one session in progress, as the node sees it. There are no byte counts
// here: a session's cost is known when it ends, and until then the only honest
// figures are who and since when.
//
// IP is carried because the link is sealed and reaches one caller. What the
// caller does with it is not the same question: it belongs in a manager's
// private reply, and never in a channel feed.
type Live struct {
	Name  string    `json:"name"`
	UUID  string    `json:"uuid"`
	IP    string    `json:"ip,omitempty"`
	Since time.Time `json:"since"`
}

// Sessions is what a node can say about who is logged in right now. proxyd's
// registry is one; a node with no ingress has nothing to implement it with, and
// passes nil.
type Sessions interface {
	Live() []Live
}

// Err is the reply as an error: nil when it succeeded.
func (r Reply) Err() error {
	if r.OK {
		return nil
	}
	return errors.New(r.Error)
}

// Resolver is what an add uses to complete a half-given player. *mojang.Client
// is one.
type Resolver interface {
	LookupName(name string) (uuid, canonical string, err error)
	LookupUUID(uuid string) (name string, err error)
}

// Server answers requests against one list.
type Server struct {
	list   *whitelist.List
	mojang Resolver // nil: an add has to carry both name and uuid
	live   Sessions // nil: this node cannot answer the sessions op
	allow  []netip.Prefix
	seal   *sealed.Sealer
}

// NewServer prepares a server. Loopback may always connect; allow is who else
// may, which is the bot's node. That is a TCP address, so trusting it is sound in
// a way it would not be over UDP, and the key is on top of it, not instead.
func NewServer(list *whitelist.List, key []byte, allow []netip.Prefix, r Resolver, live Sessions) (*Server, error) {
	s, err := sealed.NewSealer(key)
	if err != nil {
		return nil, err
	}
	return &Server{list: list, mojang: r, live: live, allow: allow, seal: s}, nil
}

// Serve answers on ln until it is closed.
func (s *Server) Serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("control: accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	if !s.allowed(c.RemoteAddr()) {
		log.Printf("control: reject %s", c.RemoteAddr())
		return
	}
	c.SetDeadline(time.Now().Add(exchangeTimeout))

	chal := make([]byte, sealed.ChallengeLen)
	rand.Read(chal)
	if _, err := c.Write(chal); err != nil {
		return
	}
	wire, err := sealed.ReadFrame(c)
	if err != nil {
		log.Printf("control: %s: %v", c.RemoteAddr(), err)
		return
	}
	// A frame that does not open was sealed with another key, or for another
	// connection. Either way it gets nothing back, not even a refusal: a reply
	// would tell a guesser that something is listening for this shape of request.
	plain, err := s.seal.Open(wire, sealed.AAD(chal, sealed.DirRequest))
	if err != nil {
		log.Printf("control: %s: refused: %v", c.RemoteAddr(), err)
		return
	}

	var rep Reply
	var req Request
	if err := json.Unmarshal(plain, &req); err != nil {
		rep = fail("bad request: " + err.Error())
	} else {
		rep = s.apply(req)
		log.Printf("control: %s: %s -> %s", c.RemoteAddr(), strings.TrimSpace(req.Op+" "+req.Name+" "+req.UUID), describe(rep))
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return
	}
	sealed.WriteFrame(c, s.seal.Seal(b, sealed.AAD(chal, sealed.DirReply)))
}

func (s *Server) allowed(a net.Addr) bool {
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
	for _, p := range s.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// namePattern is what Mojang accepts today. Legacy names outside it exist; add
// those by uuid.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

func (s *Server) apply(req Request) Reply {
	switch req.Op {
	case "list":
		return Reply{OK: true, Entries: s.list.Entries()}
	case "sessions":
		if s.live == nil {
			return fail("this node relays no sessions")
		}
		return Reply{OK: true, Live: s.live.Live()}
	case "add":
		return s.add(req)
	case "remove":
		if req.UUID == "" {
			return fail("remove needs a uuid")
		}
		e, err := s.list.Remove(req.UUID)
		if errors.Is(err, whitelist.ErrNotListed) {
			return fail("not listed")
		}
		if err != nil {
			return fail(err.Error())
		}
		return Reply{OK: true, Entry: &e}
	}
	return fail(fmt.Sprintf("unknown op %q", req.Op))
}

func (s *Server) add(req Request) Reply {
	name, uuid := strings.TrimSpace(req.Name), strings.TrimSpace(req.UUID)
	switch {
	case name == "" && uuid == "":
		return fail("add needs a name or a uuid")
	case uuid == "":
		if !namePattern.MatchString(name) {
			return fail(fmt.Sprintf("%q is not a Minecraft name", name))
		}
		if s.mojang == nil {
			return fail("this node cannot resolve names; give a uuid")
		}
		var err error
		uuid, name, err = s.mojang.LookupName(name)
		if errors.Is(err, mojang.ErrNoSuchPlayer) {
			return fail(fmt.Sprintf("no such player: %s", req.Name))
		}
		if err != nil {
			return fail(err.Error())
		}
	case name == "":
		if !IsUUID(uuid) {
			return fail(fmt.Sprintf("%q is not a uuid", uuid))
		}
		if s.mojang == nil {
			return fail("this node cannot resolve uuids; give a name too")
		}
		var err error
		name, err = s.mojang.LookupUUID(uuid)
		if errors.Is(err, mojang.ErrNoSuchPlayer) {
			return fail(fmt.Sprintf("no such player: %s", uuid))
		}
		if err != nil {
			return fail(err.Error())
		}
	}
	uuid = dashed(uuid)
	err := s.list.Add(name, uuid, req.Tag)
	var le *whitelist.ListedError
	if errors.As(err, &le) {
		e := le.Entry
		return Reply{Listed: true, Entry: &e, Error: err.Error()}
	}
	if err != nil {
		return fail(err.Error())
	}
	return Reply{OK: true, Entry: &whitelist.Entry{Name: name, UUID: uuid, Tag: strings.TrimSpace(req.Tag)}}
}

func fail(msg string) Reply { return Reply{Error: msg} }

func describe(r Reply) string {
	switch {
	case r.OK && r.Live != nil:
		return fmt.Sprintf("ok %d live", len(r.Live))
	case r.OK && r.Entry != nil:
		return "ok " + r.Entry.Name + ":" + r.Entry.UUID
	case r.OK:
		return fmt.Sprintf("ok %d entries", len(r.Entries))
	default:
		return "refused: " + r.Error
	}
}

// IsUUID reports whether s is a Minecraft UUID, dashed or bare, any case.
func IsUUID(s string) bool {
	k := strings.ToLower(strings.ReplaceAll(s, "-", ""))
	return len(k) == 32 && strings.TrimLeft(k, "0123456789abcdef") == ""
}

// dashed writes a uuid the way the file conventionally has them. Mojang answers
// with bare hex; both parse, but a file that mixes the two reads badly.
func dashed(uuid string) string {
	k := strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
	if len(k) != 32 {
		return uuid
	}
	return k[:8] + "-" + k[8:12] + "-" + k[12:16] + "-" + k[16:20] + "-" + k[20:]
}

// Find returns the entry a name or uuid refers to. Names match without regard to
// case; uuids with or without dashes.
func Find(entries []whitelist.Entry, nameOrUUID string) (whitelist.Entry, bool) {
	if IsUUID(nameOrUUID) {
		want := dashed(nameOrUUID)
		for _, e := range entries {
			if dashed(e.UUID) == want {
				return e, true
			}
		}
		return whitelist.Entry{}, false
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, nameOrUUID) {
			return e, true
		}
	}
	return whitelist.Entry{}, false
}
