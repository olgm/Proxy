package main

import (
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/probe"
	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/version"
	"github.com/olgm/proxy/internal/webhook"
)

const (
	// statusEvery is how often each node is dialled, and statusStrikes how many
	// dials in a row have to agree before anything is posted. One dropped packet
	// is not an outage; a minute of them is.
	statusEvery   = 20 * time.Second
	statusStrikes = 3

	// boardEvery is how often the card is redrawn when nothing has changed. It
	// is long because a quiet redraw is worth very little: the card is a fresh
	// upload every time, and nothing on a quiet chain moves enough in ten
	// minutes to be worth 144 of them a day. Anything actually worth knowing is
	// a transition, and a transition does not wait for this — it redraws the
	// card the moment it is announced.
	boardEvery = 10 * time.Minute

	// staleWindows is how many windows old a latency may be before the card
	// stops showing it. A figure with no window behind it any more is not a
	// measurement, it is the last thing that was measured.
	staleWindows = 3
)

// reach is what one dial found. The distinction that matters is between a host
// that answered and a host that did not: a refused connection is the kernel
// saying "I am here and nothing is on that port", which is a service being down.
// Silence is the node itself being unreachable.
type reach int

const (
	reachOK reach = iota
	reachRefused
	reachUnreachable
	reachBroken // connected, but the sealed exchange failed
)

func classify(err error) reach {
	switch {
	case err == nil:
		return reachOK
	case errors.Is(err, syscall.ECONNREFUSED):
		return reachRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return reachUnreachable
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return reachUnreachable
	}
	// We got far enough to be talking and it still failed: a wrong key, or a
	// service that accepted and then hung up.
	return reachBroken
}

// poster is the part of a webhook the status channel uses. It is an interface
// and not the queue the other feeds post through, because this feed cares about
// order and about ids: the card is deleted, then the news is posted, then the
// card goes back at the foot. A queue coalesces, and coalescing is exactly what
// would leave the card sitting above the line that explains it.
type poster interface {
	Send(webhook.Message) (string, error)
	Update(id string, m webhook.Message) error
	Delete(id string) error
}

// snap is one node as one tick found it.
type snap struct {
	node string
	live []control.Live
	err  error
	// probed is only asked about where a health link was deployed. hasProbe
	// false is "nothing here to ask", which is not the same as "asked and got
	// nothing".
	hasProbe  bool
	health    probe.Health
	healthErr error
}

// watcher owns the status channel: the card at the foot of it, and the log of
// transitions above. Only the bot can write either. A node that is down cannot
// report that it is down, and proxyd's control link says nothing about probed —
// the two services still do not know about each other.
type watcher struct {
	feed   poster
	ch     *chain
	probes map[string]*probe.HealthClient
	order  []string
	st     *store
	// ping is the role a transition notifies, or empty for a channel nobody is
	// on call for.
	ping string

	// state is the last condition announced per node, and strikes counts how
	// many dials in a row have disagreed with it.
	state   map[string]string
	strikes map[string]int
	seen    map[string]bool

	// badSince is when a node's dials started failing, which is not the same as
	// when the fault was announced: the card counts from the first bad dial, the
	// log line from the third.
	badSince map[string]time.Time
	// classes is the last class count each node reported, so a node whose probed
	// is down still contributes to the card's denominator. They are configured;
	// they are just not being measured right now.
	classes map[string]int

	board string    // the card's message id
	drawn time.Time // when it was last redrawn
}

func newWatcher(ch *chain, cfg *botcfg.Config, st *store) *watcher {
	url := os.Getenv(botcfg.EnvStatusWebhook)
	if url == "" {
		return nil
	}
	probes := map[string]*probe.HealthClient{}
	for _, p := range cfg.Probes {
		key, err := tunnel.DecodeKey(p.Key)
		if err != nil {
			log.Printf("status: %s: %v", p.Node, err)
			continue
		}
		probes[p.Node] = &probe.HealthClient{Addr: p.Addr, Key: key}
	}
	w := &watcher{
		feed: webhook.New(url), ch: ch, probes: probes,
		order:    cfg.OnlineNodes,
		st:       st,
		ping:     cfg.StatusPing,
		state:    map[string]string{},
		strikes:  map[string]int{},
		seen:     map[string]bool{},
		badSince: map[string]time.Time{},
		classes:  map[string]int{},
	}
	// Faults announced before this process started are adopted rather than
	// re-announced: a restart is not an outage, and a channel that repeats every
	// open fault on every deploy is one nobody reads.
	prev := st.get()
	w.board = prev.BoardMessage
	for node, is := range prev.Issues {
		w.seen[node], w.state[node] = true, is.Cond
	}
	return w
}

func (w *watcher) run() {
	if w == nil {
		return
	}
	for {
		w.tick(time.Now())
		time.Sleep(statusEvery)
	}
}

func (w *watcher) tick(now time.Time) { w.apply(w.look(), now) }

// apply is the whole decision: what changed, what gets said, and where the card
// ends up. It takes the snapshots rather than fetching them so that a test can
// hand it an outage.
func (w *watcher) apply(snaps []snap, now time.Time) {
	var news []change
	for _, s := range snaps {
		w.mark(s, now)
		if c, ok := w.observe(s.node, condition(s), now); ok {
			news = append(news, c)
		}
	}
	// The card is deleted before the news and reposted after it, so that it
	// stays the last message in the channel. Editing it in place would leave it
	// above lines that were posted later, and the whole point of a card at the
	// foot is that it is the thing you see first.
	if len(news) > 0 {
		w.dropCard()
		for _, c := range news {
			w.announce(c, now)
		}
	}
	w.drawCard(snaps, now, len(news) > 0)
}

// look dials every node once. Every node is asked even when one of them fails: a
// single unreachable node must not cost the card the other three.
func (w *watcher) look() []snap {
	es := w.ch.entries()
	by := make([]snap, len(es))
	var wg sync.WaitGroup
	for i, e := range es {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := snap{node: e.node}
			s.live, s.err = e.c.Sessions()
			if p, ok := w.probes[e.node]; ok {
				s.hasProbe = true
				s.health, s.healthErr = p.Check()
			}
			by[i] = s
		}()
	}
	wg.Wait()

	index := map[string]snap{}
	for _, s := range by {
		index[s.node] = s
	}
	out := make([]snap, 0, len(by))
	for _, name := range sortNodes(namesOf(w.ch), w.order) {
		out = append(out, index[name])
	}
	return out
}

// condition is what is wrong with one node, as one line, or empty when nothing
// is. probed is only asked about when the node answered at all — otherwise an
// unreachable node would report twice for one fault.
func condition(s snap) string {
	var faults []string
	switch classify(s.err) {
	case reachUnreachable:
		// No "unreachable" here: the line this ends up in already opens with
		// "is offline", and the two together read as one word said twice.
		return "no answer from the node at all"
	case reachRefused:
		faults = append(faults, "proxyd is down (the node answers, its control port does not)")
	case reachBroken:
		faults = append(faults, "proxyd is not answering (the port is open, the exchange failed)")
	}
	if s.hasProbe {
		switch classify(s.healthErr) {
		case reachRefused, reachUnreachable:
			faults = append(faults, "probed is down")
		case reachBroken:
			faults = append(faults, "probed is not answering")
		}
	}
	return strings.Join(faults, "; ")
}

// mark records what this tick saw, whether or not it is worth announcing yet.
func (w *watcher) mark(s snap, now time.Time) {
	if classify(s.err) == reachUnreachable {
		if w.badSince[s.node].IsZero() {
			w.badSince[s.node] = now
		}
		return
	}
	delete(w.badSince, s.node)
	if s.hasProbe && s.healthErr == nil {
		w.classes[s.node] = s.health.Classes
	}
}

// change is a transition worth a line in the channel.
type change struct {
	node string
	from string
	to   string
}

// observe records what was seen and reports a change that has held. The first
// observation of a node is adopted without a strike, but is still announced when
// it is a fault: a node already down when the bot started is news.
func (w *watcher) observe(node, cond string, now time.Time) (change, bool) {
	if !w.seen[node] {
		w.seen[node], w.state[node] = true, cond
		if cond == "" {
			return change{}, false
		}
		return change{node: node, to: cond}, true
	}
	if cond == w.state[node] {
		w.strikes[node] = 0
		return change{}, false
	}
	w.strikes[node]++
	if w.strikes[node] < statusStrikes {
		return change{}, false
	}
	w.strikes[node] = 0
	from := w.state[node]
	w.state[node] = cond
	return change{node: node, from: from, to: cond}, true
}

// announce posts one transition. A fault is left standing in full; a recovery
// closes the incident, and both halves of a closed incident are quietened down
// to a log line, because an incident that is over should not look like one that
// is not.
func (w *watcher) announce(c change, now time.Time) {
	line := statusLine(c.node, c.to)
	open := w.st.get().Issues[c.node]

	id, err := w.feed.Send(webhook.Message{Content: w.mention(line), Ping: w.pings()})
	if err != nil {
		log.Printf("status: %s: %v", c.node, err)
		return
	}
	// A fault replacing a different fault leaves the first line standing
	// otherwise: it has been superseded, and only one of the two is open.
	if open.Message != "" {
		w.quieten(open.Message, statusLine(c.node, open.Cond))
	}
	if c.to != "" {
		w.st.update(func(s *state) {
			s.Issues[c.node] = issue{Message: id, Cond: c.to, Since: now}
		})
		return
	}
	w.quieten(id, line)
	w.st.update(func(s *state) { delete(s.Issues, c.node) })
}

// quieten rewrites a line as subtext inside a quote, which Discord draws small
// and grey. Editing never notifies anyone, so the ping that went with the
// original is not repeated.
func (w *watcher) quieten(id, line string) {
	if id == "" {
		return
	}
	if err := w.feed.Update(id, webhook.Message{Content: "> -# " + line}); err != nil && err != webhook.ErrGone {
		log.Printf("status: quieten %s: %v", id, err)
	}
}

// statusLine is one transition as the channel reads it. The node is inside a
// code span because that is the one span Discord parses nothing inside, and the
// condition follows it so that "offline" is never the whole story.
func statusLine(node, cond string) string {
	if cond == "" {
		return "`" + node + "` is online"
	}
	return "`" + node + "` is offline — " + cond
}

func (w *watcher) mention(line string) string {
	if w.ping == "" {
		return line
	}
	return "<@&" + w.ping + "> " + line
}

// pings is the one role this feed may notify. Nothing else is ever allowed
// through, including anything in a condition that happens to look like a
// mention.
func (w *watcher) pings() []string {
	if w.ping == "" {
		return nil
	}
	return []string{w.ping}
}

// dropCard removes the card so the news can be posted below where it was. A
// delete that fails is logged and the id forgotten anyway: a stale card left in
// the channel is worse than the alternative, which is editing a card that is now
// above the news.
func (w *watcher) dropCard() {
	if w.board == "" {
		return
	}
	if err := w.feed.Delete(w.board); err != nil {
		log.Printf("status: delete card: %v", err)
	}
	w.board = ""
	w.st.update(func(s *state) { s.BoardMessage = "" })
}

// drawCard redraws the card. With nothing to report it is edited in place, which
// costs one upload and keeps it where it is; after news it is posted fresh,
// because dropCard has just removed it.
func (w *watcher) drawCard(snaps []snap, now time.Time, force bool) {
	if !force && now.Sub(w.drawn) < boardEvery {
		return
	}
	png, err := renderCard(w.card(snaps, now))
	if err != nil {
		log.Printf("status: draw: %v", err)
		return
	}
	msg := webhook.Message{Image: &webhook.Image{Name: "status.png", PNG: png}}

	if w.board != "" {
		switch err := w.feed.Update(w.board, msg); err {
		case nil:
			w.drawn = now
			return
		case webhook.ErrGone:
			log.Printf("status: the card is gone; posting a new one")
		default:
			log.Printf("status: update card: %v", err)
			return
		}
	}
	id, err := w.feed.Send(msg)
	if err != nil {
		log.Printf("status: post card: %v", err)
		return
	}
	w.board, w.drawn = id, now
	w.st.update(func(s *state) { s.BoardMessage = id })
}

// card turns a tick into the picture of it.
func (w *watcher) card(snaps []snap, now time.Time) cardData {
	d := cardData{version: version.String(), at: now, nodesTotal: len(snaps)}
	issues := w.st.get().Issues
	for _, s := range snaps {
		n := cardNode{node: s.node}
		switch classify(s.err) {
		case reachOK:
			n.proxyd = true
			n.players, n.playersKnown = len(s.live), true
			d.online += n.players
		case reachUnreachable:
			n.health = healthDown
			n.downFor = w.downFor(s.node, issues, now)
		}
		if n.health != healthDown && s.hasProbe {
			n.probedKnown = true
			n.probed = s.healthErr == nil
			if n.probed {
				n.leg, n.ms = pickLeg(s.health.Legs)
				d.classesUp += s.health.Classes
			}
		}
		if n.health != healthDown && (!n.proxyd || (n.probedKnown && !n.probed)) {
			n.health = healthDegraded
		}
		if n.health == healthUp {
			d.nodesUp++
		}
		d.classesTotal += w.classes[s.node]
		d.nodes = append(d.nodes, n)
	}
	return d
}

// downFor is how long a node has been unreachable. The announced fault's age is
// preferred where there is one, because it survives a restart of this process
// and the dial history does not.
func (w *watcher) downFor(node string, issues map[string]issue, now time.Time) time.Duration {
	if is, ok := issues[node]; ok && !is.Since.IsZero() {
		return now.Sub(is.Since)
	}
	return now.Sub(w.badSince[node])
}

// pickLeg is the one measurement a node's row shows: the furthest path it
// originates, which is that node's own route to the exit and the number anyone
// asking "how is hk doing" means. A chain beats a leg even when both name two
// nodes — "hk>ch" is the whole route through Tokyo and "hk>ty" is its first
// hop — so the kind decides before the length does. A node that originates
// nothing is the exit and has no leg at all.
func pickLeg(legs []probe.Leg) (string, float64) {
	var best probe.Leg
	for _, l := range legs {
		if best.Class == "" || further(l, best) {
			best = l
		}
	}
	if best.Class == "" {
		return "", -1
	}
	name := strings.ReplaceAll(best.Class, ">", "→")
	// N and not P50: a window in which every probe was lost still closes, with no
	// round trips behind it and a percentile of zero. Drawing that would put
	// "0.0 ms" against a leg that carried nothing, which is the most wrong a
	// latency can be.
	if best.N <= 0 || stale(best) {
		return name, -1
	}
	return name, best.P50
}

// further ranks one class above another: a chain over a leg, then more hops,
// then by name so that two of equal reach always pick the same one.
func further(a, b probe.Leg) bool {
	if (a.Kind == probe.KindChain) != (b.Kind == probe.KindChain) {
		return a.Kind == probe.KindChain
	}
	if na, nb := strings.Count(a.Class, ">"), strings.Count(b.Class, ">"); na != nb {
		return na > nb
	}
	return a.Class < b.Class
}

// stale is a figure whose window closed too long ago to still be describing the
// leg. A probed that stopped measuring keeps reporting its last window forever,
// and a card that showed it would be quietly wrong rather than loudly unknown.
func stale(l probe.Leg) bool {
	w, err := time.ParseDuration(l.Window)
	if err != nil {
		return true
	}
	return time.Duration(l.AgeSecs)*time.Second > staleWindows*w
}

func (w *watcher) Close() {}

// namesOf is every entry the chain holds, as the roster's sorter wants them.
func namesOf(ch *chain) []nodeLive {
	es := ch.entries()
	out := make([]nodeLive, 0, len(es))
	for _, e := range es {
		out = append(out, nodeLive{node: e.node})
	}
	return out
}
