package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/probe"
	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/webhook"
)

const (
	// statusEvery is how often each node is dialled, and statusStrikes how many
	// dials in a row have to agree before anything is posted. One dropped packet
	// is not an outage; a minute of them is.
	statusEvery   = 20 * time.Second
	statusStrikes = 3
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

// watcher posts node and service transitions. Only the bot can: a node that is
// down cannot report that it is down, and proxyd's link says nothing about
// probed — the two services still do not know about each other.
type watcher struct {
	q *webhook.Queue
	// send is where a line goes. It is a field so a test can watch the decisions
	// without standing up a webhook: what is worth testing here is when
	// something is said, not how it is delivered.
	send   func(string)
	ch     *chain
	probes map[string]*probe.HealthClient
	order  []string

	// state is the last condition announced per node, and strikes counts how
	// many dials in a row have disagreed with it.
	state   map[string]string
	strikes map[string]int
	seen    map[string]bool
}

func newWatcher(ch *chain, cfg *botcfg.Config) *watcher {
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
		q: webhook.NewQueue(url, 64, 2*time.Second), ch: ch, probes: probes,
		order:   cfg.OnlineNodes,
		state:   map[string]string{},
		strikes: map[string]int{},
		seen:    map[string]bool{},
	}
	w.send = w.q.Send
	return w
}

func (w *watcher) run() {
	if w == nil {
		return
	}
	for {
		w.tick()
		time.Sleep(statusEvery)
	}
}

func (w *watcher) tick() {
	for _, node := range sortNodes(namesOf(w.ch), w.order) {
		w.observe(node, w.condition(node))
	}
}

// condition is what is wrong with one node right now, as one line, or empty when
// nothing is. Reachability is decided by the control dial, and probed is only
// asked about when the node answered at all — otherwise an unreachable node
// would report twice for one fault.
func (w *watcher) condition(node string) string {
	e, ok := w.entry(node)
	if !ok {
		return ""
	}
	var faults []string
	switch classify(errOf(e.c.Sessions())) {
	case reachUnreachable:
		return "unreachable — no answer from the node at all"
	case reachRefused:
		faults = append(faults, "proxyd is down (the node answers, its control port does not)")
	case reachBroken:
		faults = append(faults, "proxyd is not answering (the port is open, the exchange failed)")
	}
	if p, ok := w.probes[node]; ok {
		_, err := p.Check()
		switch classify(err) {
		case reachRefused, reachUnreachable:
			faults = append(faults, "probed is down")
		case reachBroken:
			faults = append(faults, "probed is not answering")
		}
	}
	return strings.Join(faults, "; ")
}

// observe records what was seen and posts only a change that has held. The first
// observation of a node is adopted without a strike, but is still announced when
// it is a fault: a node already down when the bot started is news.
func (w *watcher) observe(node, cond string) {
	if !w.seen[node] {
		w.seen[node], w.state[node] = true, cond
		if cond != "" {
			w.post(node, cond)
		}
		return
	}
	if cond == w.state[node] {
		w.strikes[node] = 0
		return
	}
	w.strikes[node]++
	if w.strikes[node] < statusStrikes {
		return
	}
	w.strikes[node] = 0
	w.state[node] = cond
	w.post(node, cond)
}

func (w *watcher) post(node, cond string) {
	if cond == "" {
		w.send(fmt.Sprintf("**%s** recovered", node))
		return
	}
	w.send(fmt.Sprintf("**%s** %s", node, cond))
}

func (w *watcher) entry(node string) (entry, bool) {
	for _, e := range w.ch.entries() {
		if e.node == node {
			return e, true
		}
	}
	return entry{}, false
}

func (w *watcher) Close() {
	if w != nil {
		w.q.Close()
	}
}

// errOf keeps the call sites above to one line; only the error is interesting.
func errOf[T any](_ T, err error) error { return err }

// namesOf is every entry the chain holds, as the roster's sorter wants them.
func namesOf(ch *chain) []nodeLive {
	es := ch.entries()
	out := make([]nodeLive, 0, len(es))
	for _, e := range es {
		out = append(out, nodeLive{node: e.node})
	}
	return out
}
