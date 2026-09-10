package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/webhook"
)

// rosterEvery is how often the entries are asked who they are relaying. The
// message is only edited when the answer changed, so this is a polling cost and
// not an editing one.
const rosterEvery = 20 * time.Second

// roster keeps one message showing who is online across every entry. Only the
// bot can build it: a node knows its own sessions and no others.
type roster struct {
	ch    *chain
	c     *webhook.Client
	order []string
	st    *store

	id   string
	last string // the last body posted, so an unchanged roster is not re-edited
}

func newRoster(ch *chain, order []string, st *store) *roster {
	url := os.Getenv(botcfg.EnvOnlineWebhook)
	if url == "" {
		return nil
	}
	return &roster{ch: ch, c: webhook.New(url), order: order, st: st, id: st.get().OnlineMessage}
}

// run redraws the roster forever. Nil when no webhook was configured, and a nil
// roster is simply never started.
func (r *roster) run() {
	if r == nil {
		return
	}
	for {
		r.tick()
		time.Sleep(rosterEvery)
	}
}

func (r *roster) tick() {
	body := render(r.ch.sessions(), r.order)
	if body == r.last {
		return // nothing moved; an unchanged roster is still a correct one
	}
	if err := r.publish(body); err != nil {
		log.Printf("roster: %v", err)
		return
	}
	r.last = body
}

// publish edits the message it has, and posts a new one when there is none or
// when the one it had is gone — somebody cleared the channel, or deleted it.
func (r *roster) publish(body string) error {
	if r.id != "" {
		err := r.c.Edit(r.id, body)
		if err == nil {
			return nil
		}
		if err != webhook.ErrGone {
			return err
		}
		log.Printf("roster: the message is gone; posting a new one")
	}
	id, err := r.c.Post(body)
	if err != nil {
		return err
	}
	r.id = id
	r.st.update(func(s *state) { s.OnlineMessage = id })
	return nil
}

// render draws the roster, grouped by the entry a player arrived at and in the
// order the topology asked for. A node that could not be reached says so: it is
// not the same fact as nobody being online there.
func render(nodes []nodeLive, order []string) string {
	by := map[string]nodeLive{}
	for _, n := range nodes {
		by[n.node] = n
	}

	total := 0
	var b strings.Builder
	var lines []string
	for _, name := range sortNodes(nodes, order) {
		n := by[name]
		switch {
		case n.err != nil:
			lines = append(lines, fmt.Sprintf("**%s** unreachable", name))
		case len(n.live) == 0:
			// A quiet entry is not worth a line; the header carries the total.
		default:
			total += len(n.live)
			lines = append(lines, fmt.Sprintf("**%s** %s", name, playerList(n.live)))
		}
	}

	fmt.Fprintf(&b, "**Online — %d**\n", total)
	if len(lines) == 0 {
		b.WriteString("\nnobody")
	} else {
		b.WriteString("\n" + strings.Join(lines, "\n"))
	}
	// A Discord relative timestamp keeps counting up on its own, so a roster
	// that has not changed in an hour says so without being edited again.
	fmt.Fprintf(&b, "\n\n_changed <t:%d:R>_", time.Now().Unix())
	return b.String()
}

func playerList(live []control.Live) string {
	out := make([]string, 0, len(live))
	for _, s := range live {
		out = append(out, fmt.Sprintf("`%s` %s", ign(s.Name), since(s.Since)))
	}
	return strings.Join(out, " · ")
}

// sortNodes puts the entries the topology named first, in that order, then
// everything else in the order the chain holds it — so an entry added later
// appears at the end rather than not at all.
func sortNodes(nodes []nodeLive, order []string) []string {
	rank := map[string]int{}
	for i, n := range order {
		rank[n] = i
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.node)
	}
	sort.SliceStable(names, func(i, j int) bool {
		ri, oki := rank[names[i]]
		rj, okj := rank[names[j]]
		if oki != okj {
			return oki
		}
		if oki {
			return ri < rj
		}
		return false
	})
	return names
}

// since renders how long a session has been up, at the resolution anyone reads
// it at. A session that started in the future is a clock skew, not a negative.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ign keeps a name inside its own inline-code span. Discord parses nothing
// inside one, which is what an IGN containing an underscore needs.
func ign(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '`' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "?"
	}
	return name
}
