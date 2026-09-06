package main

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/tunnel"
)

// entry is one whitelisted entry node, reached over its control link.
type entry struct {
	node string
	c    *control.Client
}

// chain is every entry the bot manages. Every whitelisted entry carries the same
// membership: a player is whitelisted on the chain, not on a node. Two writes can
// never be atomic, so the primary is the tie-break: an op goes there first and
// stops if it is refused, and reconcile makes every other entry match it.
type chain struct {
	primary entry
	others  []entry
}

func newChain(cfg *botcfg.Config) (*chain, error) {
	ch := &chain{}
	for _, e := range cfg.Entries {
		key, err := tunnel.DecodeKey(e.Key)
		if err != nil {
			return nil, fmt.Errorf("entry %s: %w", e.Node, err)
		}
		en := entry{node: e.Node, c: &control.Client{Addr: e.Addr, Key: key}}
		if e.Node == cfg.Primary {
			ch.primary = en
		} else {
			ch.others = append(ch.others, en)
		}
	}
	if ch.primary.c == nil {
		return nil, fmt.Errorf("primary %q is not among the entries", cfg.Primary)
	}
	return ch, nil
}

// result is what an op did: the primary's answer, and the entries that did not
// take it, which reconcile will bring level.
type result struct {
	rep     control.Reply
	lagging []string
}

func (r result) lag() string {
	if len(r.lagging) == 0 {
		return ""
	}
	return fmt.Sprintf(" It has not reached %s yet; it will within a few minutes.", strings.Join(r.lagging, ", "))
}

// add lists a player everywhere. The primary resolves the name; the others are
// given both halves, so only one Mojang lookup happens.
func (ch *chain) add(name, uuid, tag string) (result, error) {
	rep, err := ch.primary.c.Add(name, uuid, tag)
	if err != nil || !rep.OK {
		return result{rep: rep}, err
	}
	e := rep.Entry
	res := result{rep: rep}
	for _, o := range ch.others {
		r, err := o.c.Add(e.Name, e.UUID, e.Tag)
		if err == nil && (r.OK || r.Listed) {
			continue // listed already, perhaps under another tag: reconcile settles that
		}
		log.Printf("chain: %s: add %s: %v %s", o.node, e.Name, err, r.Error)
		res.lagging = append(res.lagging, o.node)
	}
	return res, nil
}

// remove drops a player everywhere.
func (ch *chain) remove(uuid string) (result, error) {
	rep, err := ch.primary.c.Remove(uuid)
	if err != nil || !rep.OK {
		return result{rep: rep}, err
	}
	res := result{rep: rep}
	for _, o := range ch.others {
		r, err := o.c.Remove(uuid)
		if err == nil && (r.OK || r.Error == "not listed") {
			continue
		}
		log.Printf("chain: %s: remove %s: %v %s", o.node, uuid, err, r.Error)
		res.lagging = append(res.lagging, o.node)
	}
	return res, nil
}

// nodeLive is one entry's answer to the sessions op. A node that could not be
// asked carries the error rather than an empty list: "nobody is online here" and
// "I could not reach it" are different facts and a roster that merged them would
// drop a node without saying so.
type nodeLive struct {
	node string
	live []control.Live
	err  error
}

// entries is every entry the bot manages, primary first.
func (ch *chain) entries() []entry {
	return append([]entry{ch.primary}, ch.others...)
}

// sessions asks every entry who it is relaying. Every entry is asked even when
// one of them fails: a single unreachable node must not cost the roster the
// other three.
func (ch *chain) sessions() []nodeLive {
	es := ch.entries()
	out := make([]nodeLive, len(es))
	var wg sync.WaitGroup
	for i, e := range es {
		wg.Add(1)
		go func() {
			defer wg.Done()
			live, err := e.c.Sessions()
			out[i] = nodeLive{node: e.node, live: live, err: err}
		}()
	}
	wg.Wait()
	return out
}

// nodePast is one entry's answer to the history op.
type nodePast struct {
	node string
	past []control.Past
	err  error
}

// history asks every entry for the newest n sessions belonging to any of uuids.
// Every node is asked for the whole page rather than a share of it: no node can
// page a total order it only holds part of, so the merge does the paging.
func (ch *chain) history(uuids []string, n int) []nodePast {
	es := ch.entries()
	out := make([]nodePast, len(es))
	var wg sync.WaitGroup
	for i, e := range es {
		wg.Add(1)
		go func() {
			defer wg.Done()
			past, err := e.c.History(uuids, n)
			for j := range past {
				past[j].Node = e.node // only this side knows which node it asked
			}
			out[i] = nodePast{node: e.node, past: past, err: err}
		}()
	}
	wg.Wait()
	return out
}

// list is the primary's list, which is the one that counts.
func (ch *chain) list() ([]control.Entry, error) {
	return ch.primary.c.List()
}

// reconcile makes every other entry's set of (uuid, tag) match the primary's.
// Names are left alone: each node keeps its own, renamed on login and refreshed
// daily, and they may differ for a day without meaning anything.
func (ch *chain) reconcile() {
	want, err := ch.primary.c.List()
	if err != nil {
		log.Printf("reconcile: %s: %v", ch.primary.node, err)
		return
	}
	wantBy := index(want)
	for _, o := range ch.others {
		have, err := o.c.List()
		if err != nil {
			log.Printf("reconcile: %s: %v", o.node, err)
			continue
		}
		haveBy := index(have)
		fixed := 0
		for k, e := range wantBy {
			h, ok := haveBy[k]
			if ok && h.Tag == e.Tag {
				continue
			}
			if ok {
				// Same player, another tag: the primary's says whose it is.
				if r, err := o.c.Remove(h.UUID); err != nil || !r.OK {
					log.Printf("reconcile: %s: remove %s: %v %s", o.node, h.Name, err, r.Error)
					continue
				}
			}
			if r, err := o.c.Add(e.Name, e.UUID, e.Tag); err != nil || !r.OK {
				log.Printf("reconcile: %s: add %s: %v %s", o.node, e.Name, err, r.Error)
				continue
			}
			fixed++
		}
		for k, h := range haveBy {
			if _, ok := wantBy[k]; ok {
				continue
			}
			if r, err := o.c.Remove(h.UUID); err != nil || !r.OK {
				log.Printf("reconcile: %s: remove %s: %v %s", o.node, h.Name, err, r.Error)
				continue
			}
			fixed++
		}
		if fixed > 0 {
			log.Printf("reconcile: %s: %d lines brought level with %s", o.node, fixed, ch.primary.node)
		}
	}
}

func index(es []control.Entry) map[string]control.Entry {
	out := make(map[string]control.Entry, len(es))
	for _, e := range es {
		out[bare(e.UUID)] = e
	}
	return out
}

func bare(uuid string) string { return strings.ToLower(strings.ReplaceAll(uuid, "-", "")) }
