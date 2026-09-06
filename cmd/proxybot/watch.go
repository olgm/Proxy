package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olgm/proxy/internal/control"
)

// watchPage is how many finished sessions one page shows. A session takes a
// verbose line, and a page nobody can read is not a page.
const watchPage = 5

// watchPrefix marks the paging buttons. The page number rides in the button's
// own id, so the bot holds nothing between one press and the next — the state
// is in the message the member is looking at.
const watchPrefix = "watch:"

func watchID(user string, page int) string {
	return watchPrefix + user + ":" + strconv.Itoa(page)
}

// parseWatchID reads a button back. A malformed one is somebody else's button.
func parseWatchID(id string) (user string, page int, ok bool) {
	rest, found := strings.CutPrefix(id, watchPrefix)
	if !found {
		return "", 0, false
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", 0, false
	}
	page, err := strconv.Atoi(rest[i+1:])
	if err != nil || page < 0 || rest[:i] == "" {
		return "", 0, false
	}
	return rest[:i], page, true
}

// watchResult is one rendered page and whether there is another after it.
type watchResult struct {
	text string
	user string
	page int
	more bool
}

// watch answers /watch: a member's accounts, the sessions they have open right
// now, and a page of the ones that have finished. Managers only, and always
// privately — this is the one place the client IP is reported, and one manager
// reading it is a different audience from a channel.
func (b *bot) watch(m member, user string, page int) watchResult {
	g := b.grantFor(m.roles)
	if !g.any {
		return watchResult{text: "You have no role that may use the whitelist.", user: user, page: page}
	}
	if !g.manage {
		return watchResult{text: "Only managers may watch a member.", user: user, page: page}
	}
	if user == "" {
		return watchResult{text: "Name a member to watch.", user: user, page: page}
	}

	entries, err := b.chain.list()
	if err != nil {
		return watchResult{text: unreachable, user: user, page: page}
	}
	var mine []control.Entry
	uuids := []string{}
	for _, e := range entries {
		if owner(e.Tag) == user {
			mine = append(mine, e)
			uuids = append(uuids, e.UUID)
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "**Watching <@%s>**\n", user)
	out.WriteString("\n**Accounts**\n")
	if len(mine) == 0 {
		out.WriteString("none\n")
	}
	for _, e := range mine {
		fmt.Fprintf(&out, "`%s` `%s`\n", ign(e.Name), e.UUID)
	}

	// No accounts means nothing to look up, and asking four nodes for the
	// history of an empty set would return everyone's.
	if len(uuids) == 0 {
		return watchResult{text: out.String(), user: user, page: page}
	}

	out.WriteString("\n**Online now**\n")
	if on := b.online(uuids); len(on) == 0 {
		out.WriteString("nobody\n")
	} else {
		for _, l := range on {
			fmt.Fprintf(&out, "`%s` on **%s** from `%s` · %s\n", ign(l.Name), l.Node, l.IP, since(l.Since))
		}
	}

	past, more, err := b.history(uuids, page)
	if err != nil {
		out.WriteString("\n**Sessions**\nthe nodes could not be reached\n")
		return watchResult{text: out.String(), user: user, page: page}
	}
	fmt.Fprintf(&out, "\n**Sessions** — page %d\n", page+1)
	if len(past) == 0 && page == 0 {
		out.WriteString("none recorded\n")
	}
	for _, p := range past {
		out.WriteString(sessionLine(p) + "\n")
	}
	return watchResult{text: out.String(), user: user, page: page, more: more}
}

// liveAt is one open session with the node it is on.
type liveAt struct {
	control.Live
	Node string
}

// online is the member's open sessions, across every entry. A node that could
// not be asked is skipped: a partial answer beats none, and the status feed is
// what reports a node being down.
func (b *bot) online(uuids []string) []liveAt {
	want := map[string]bool{}
	for _, u := range uuids {
		want[bare(u)] = true
	}
	var out []liveAt
	for _, n := range b.chain.sessions() {
		if n.err != nil {
			continue
		}
		for _, l := range n.live {
			if want[bare(l.UUID)] {
				out = append(out, liveAt{Live: l, Node: n.node})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// history merges every node's newest sessions into one order and cuts the page
// out of it. Each node is asked for the whole prefix rather than a share,
// because none of them can page an order it only holds part of.
func (b *bot) history(uuids []string, page int) ([]control.Past, bool, error) {
	// One extra, so "is there another page" is answered without a second round
	// trip to four nodes.
	want := (page+1)*watchPage + 1
	var all []control.Past
	reached := false
	for _, n := range b.chain.history(uuids, want) {
		if n.err != nil {
			continue
		}
		reached = true
		all = append(all, n.past...)
	}
	if !reached {
		return nil, false, fmt.Errorf("no entry could be reached")
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].End.After(all[j].End) })

	start := page * watchPage
	if start >= len(all) {
		return nil, false, nil
	}
	end := min(start+watchPage, len(all))
	return all[start:end], len(all) > end, nil
}

// sessionLine is one finished session: who, where from, when, and what it cost.
func sessionLine(p control.Past) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` **%s** `%s` · %s → %s (%s) · up %s down %s",
		ign(p.Name), p.Node, p.IP,
		p.Start.UTC().Format("2 Jan 15:04"), p.End.UTC().Format("15:04"),
		p.End.Sub(p.Start).Round(time.Second),
		size(p.Up), size(p.Down))
	if p.Wire > 0 {
		fmt.Fprintf(&b, " wire %s", size(int64(p.Wire)))
		if payload := p.Up + p.Down; payload > 0 {
			fmt.Fprintf(&b, " ×%.2f", float64(p.Wire)/float64(payload))
		}
	}
	return b.String()
}

// size renders a byte count the way an operator reads one, as proxyd's own log
// line does.
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
