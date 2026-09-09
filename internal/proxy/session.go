package proxy

import (
	"fmt"
	"strings"
	"time"

	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/webhook"
)

// EnvSessionsWebhook names the environment variable holding the webhook URL for
// the session feed. proxyctl writes it into the node's env file from whatever
// variable topology.json named; proxyd reads it here. Unset is the normal state
// and means the feed is off, so there is nothing else to check.
const EnvSessionsWebhook = "PROXYD_SESSIONS_WEBHOOK"

// Session is one relayed login. It is opened when a player gets past the
// whitelist and completed when the relay ends, which is the first moment the
// node knows what the session cost.
//
// IP is in here because the node has it and `/watch` reports it to a manager
// privately. It is deliberately absent from everything the feed renders: a
// channel is a different audience from an ephemeral reply to one manager.
type Session struct {
	Node  string
	IP    string
	Name  string
	UUID  string
	Proto int
	Start time.Time

	// Set when the session ends.
	End  time.Time
	Up   int64
	Down int64
	// Chain is what the session cost the fleet in traffic a VPS bills for: every
	// byte in or out of every node that carried it. See chainCost.
	Chain uint64
	// Online is this node's count of relayed logins at the moment of the event.
	// A node knows its own and no others, which is why the fleet-wide roster is
	// a separate feed that only the bot can build.
	Online int64
}

// For reports how long the session lasted, rounded the way an operator reads it.
func (s Session) For() time.Duration { return s.End.Sub(s.Start).Round(time.Second) }

// sessionFeed posts one line per login and one per logout. It is nil unless the
// node was given a webhook URL, which is what makes this something an operator
// turns on rather than something proxyd does.
type sessionFeed struct{ q *webhook.Queue }

// newSessionFeed returns nil when there is no URL, and every method below
// tolerates that: a feed nobody configured must cost the relay path nothing.
func newSessionFeed(url string) *sessionFeed {
	if url == "" {
		return nil
	}
	// Deep enough to swallow a reconnect storm, and two seconds is slow enough
	// that a storm arrives as one message rather than fifty.
	return &sessionFeed{q: webhook.NewQueue(url, 256, 2*time.Second)}
}

func (f *sessionFeed) login(s Session) {
	if f == nil {
		return
	}
	f.q.Send(fmt.Sprintf("**%s** %s joined%s · online %d", s.Node, code(s.Name), uuidPart(s.UUID), s.Online))
}

func (f *sessionFeed) logout(s Session) {
	if f == nil {
		return
	}
	line := fmt.Sprintf("**%s** %s left · %s · up %s down %s", s.Node, code(s.Name), s.For(), size(s.Up), size(s.Down))
	if s.Chain > 0 {
		line += " · chain " + size(int64(s.Chain))
	}
	f.q.Send(line + fmt.Sprintf(" · online %d", s.Online))
}

func (f *sessionFeed) Close() {
	if f != nil {
		f.q.Close()
	}
}

// code renders an IGN as inline code, which is the one span Discord does no
// markup inside. A name is allowed to contain an underscore, and `_x_` in bold
// would come out italic; a legacy name is allowed to contain worse. A route with
// no whitelist never reads Login Start at all, so it has no name to render.
func code(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '`' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "a session"
	}
	return "`" + name + "`"
}

// uuidPart is omitted rather than rendered empty on a route with no whitelist,
// where there is no Login Start to have read one from.
func uuidPart(uuid string) string {
	if uuid == "" {
		return ""
	}
	return " `" + uuid + "`"
}

// chainCost is what a session cost the fleet in traffic a VPS bills for: every
// byte in or out of every node that carried it.
//
// Two nodes are billed for each byte that crosses a tunnel leg — it leaves one
// and arrives at the other — and one node is billed at each end of the chain,
// where the far side is a player or the backend. So the two ends come to
// 2·(up+down) together, and each leg to twice what crossed it.
//
// This node measures its own leg exactly: duplicates, re-sends and the acks and
// nacks that repair it. The legs past it are reckoned to cost the same, which
// holds while every leg carries the same chunks the same number of times, as
// they all do at duplicate 2, and is out by however much their loss rates
// differ. legs is the one part of the shape a node cannot see for itself —
// Hops names the node after this one, never how many come after that.
func chainCost(u halfCloser, legs int, up, down int64) uint64 {
	ends := uint64(2 * (up + down))
	st, ok := u.(*tunnel.Stream)
	if !ok {
		return ends // a direct exit: the player's leg and the backend's, no more
	}
	if legs < 1 {
		legs = 1 // there is a stream, so there is at least the leg just measured
	}
	return ends + 2*uint64(legs)*st.Traffic().Total()
}
