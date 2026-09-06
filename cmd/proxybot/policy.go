package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/control"
)

// grant is what a member may do, from every configured role they hold; see
// botcfg.Role.
type grant struct {
	accounts int
	manage   bool
	any      bool // holds at least one configured role
}

func (b *bot) grantFor(roles []string) grant {
	var g grant
	for _, id := range roles {
		r, ok := b.cfg.Roles[id]
		if !ok {
			continue
		}
		g.any = true
		g.manage = g.manage || r.Manage
		if r.Accounts > g.accounts {
			g.accounts = r.Accounts
		}
	}
	return g
}

// tagPrefix marks a line the bot wrote. What follows is the member's id, which
// is the only thing that decides whose line it is.
const tagPrefix = "discord:"

func tagFor(userID string) string { return tagPrefix + userID }

// owner is the member a tag names, or "" for a line added some other way.
func owner(tag string) string {
	if strings.HasPrefix(tag, tagPrefix) {
		return strings.TrimPrefix(tag, tagPrefix)
	}
	return ""
}

// command is one slash command, pulled out of the interaction.
type command struct {
	op     string // add, remove, list, purge
	player string // an ign or a uuid
	user   string // a member's id, when the command names one
}

type member struct {
	id    string
	roles []string
}

// reply is what the member sees, and the line for the audit channel when the
// command changed something.
type reply struct {
	text  string
	audit string
}

func say(format string, a ...any) reply { return reply{text: fmt.Sprintf(format, a...)} }

// membersAPI is the one question the bot asks Discord outside an interaction:
// whether a member is still here, and with which roles.
type membersAPI interface {
	// lookup reports a member's roles, or present false when they are not in
	// the guild. An error means Discord could not say, which is not the same.
	lookup(userID string) (roles []string, present bool, err error)
}

type bot struct {
	cfg     *botcfg.Config
	chain   *chain
	adds    *limiter
	members membersAPI
	// roster keeps the online message up to date, and watcher posts node and
	// service transitions. Both nil when no webhook was configured.
	roster  *roster
	watcher *watcher
}

const unreachable = "The proxy could not be reached; try again in a minute."

func (b *bot) handle(m member, c command) reply {
	g := b.grantFor(m.roles)
	if !g.any {
		return say("You have no role that may use the whitelist.")
	}
	var r reply
	switch c.op {
	case "add":
		r = b.add(m, g, c)
	case "remove":
		r = b.remove(m, g, c)
	case "list":
		r = b.list(m, g, c)
	case "purge":
		r = b.purge(m, g, c)
	default:
		r = say("Unknown command.")
	}
	log.Printf("%s: %s %s %s -> %s", m.id, c.op, c.player, c.user, strings.SplitN(r.text, "\n", 2)[0])
	return r
}

func (b *bot) add(m member, g grant, c command) reply {
	target := m.id
	if c.user != "" && c.user != m.id {
		if !g.manage {
			return say("Only a manager may whitelist an account for someone else.")
		}
		target = c.user
	}
	if !g.manage {
		if !b.adds.allow(m.id) {
			return say("Slow down: a few adds a minute is plenty.")
		}
		es, err := b.chain.list()
		if err != nil {
			log.Printf("add: %v", err)
			return say(unreachable)
		}
		// Already listed under the name given: say whose, before the cap has
		// its say and before Mojang is asked. A renamed player falls through
		// and is caught by the node the same way.
		if e, ok := control.Find(es, c.player); ok {
			return listed(e, target)
		}
		if mine := len(withTag(es, tagFor(m.id))); mine >= g.accounts {
			return say("You may whitelist %d account%s and have %d. Remove one first with `/whitelist remove`.",
				g.accounts, plural(g.accounts), mine)
		}
	}
	name, uuid := c.player, ""
	if control.IsUUID(c.player) {
		name, uuid = "", c.player
	}
	res, err := b.chain.add(name, uuid, tagFor(target))
	if err != nil {
		log.Printf("add: %v", err)
		return say(unreachable)
	}
	rep := res.rep
	if rep.Listed {
		return listed(*rep.Entry, target)
	}
	if !rep.OK {
		return say("Could not add `%s`: %s", c.player, rep.Error)
	}
	e := rep.Entry
	forWhom := ""
	if target != m.id {
		forWhom = fmt.Sprintf(" for <@%s>", target)
	}
	return reply{
		text:  fmt.Sprintf("Added `%s` (`%s`)%s.%s", e.Name, e.UUID, forWhom, res.lag()),
		audit: fmt.Sprintf("<@%s> added `%s` (`%s`)%s", m.id, e.Name, e.UUID, forWhom),
	}
}

// listed is the answer to adding a player who is already on the list.
func listed(e control.Entry, target string) reply {
	switch who := owner(e.Tag); {
	case who == target:
		return say("`%s` is already on the whitelist for <@%s>.", e.Name, target)
	case who != "":
		return say("`%s` is already whitelisted by <@%s>.", e.Name, who)
	default:
		return say("`%s` is already whitelisted by the operator.", e.Name)
	}
}

func (b *bot) remove(m member, g grant, c command) reply {
	es, err := b.chain.list()
	if err != nil {
		log.Printf("remove: %v", err)
		return say(unreachable)
	}
	e, ok := control.Find(es, c.player)
	if !ok {
		return say("`%s` is not whitelisted.", c.player)
	}
	who := owner(e.Tag)
	if who != m.id && !g.manage {
		return say("`%s` was not whitelisted by you.", e.Name)
	}
	res, err := b.chain.remove(e.UUID)
	if err != nil {
		log.Printf("remove: %v", err)
		return say(unreachable)
	}
	if !res.rep.OK {
		return say("Could not remove `%s`: %s", e.Name, res.rep.Error)
	}
	whose := ""
	switch {
	case who != "" && who != m.id:
		whose = fmt.Sprintf(", whitelisted by <@%s>", who)
	case who == "" && e.Tag != "":
		whose = fmt.Sprintf(", tagged `%s`", e.Tag)
	case who == "":
		whose = ", added by the operator"
	}
	return reply{
		text:  fmt.Sprintf("Removed `%s`.%s", e.Name, res.lag()),
		audit: fmt.Sprintf("<@%s> removed `%s` (`%s`)%s", m.id, e.Name, e.UUID, whose),
	}
}

func (b *bot) list(m member, g grant, c command) reply {
	es, err := b.chain.list()
	if err != nil {
		log.Printf("list: %v", err)
		return say(unreachable)
	}
	var show []control.Entry
	header, owners := "", false
	switch {
	case c.user != "" && c.user != m.id:
		if !g.manage {
			return say("Only a manager may see someone else's accounts.")
		}
		show, header = withTag(es, tagFor(c.user)), fmt.Sprintf("<@%s>'s accounts", c.user)
	case g.manage && c.user == "":
		show, header, owners = es, "Every whitelisted account", true
	default:
		show, header = withTag(es, tagFor(m.id)), "Your accounts"
	}
	if len(show) == 0 {
		return say("%s: none.", header)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%d):\n", header, len(show))
	for i, e := range show {
		line := fmt.Sprintf("• `%s` `%s`", e.Name, e.UUID)
		if owners {
			switch who := owner(e.Tag); {
			case who != "":
				line += fmt.Sprintf(" — <@%s>", who)
			case e.Tag != "":
				line += fmt.Sprintf(" — `%s`", e.Tag)
			default:
				line += " — operator"
			}
		}
		// Discord stops reading at 2000 characters.
		if sb.Len()+len(line) > 1900 {
			fmt.Fprintf(&sb, "… and %d more", len(show)-i)
			break
		}
		sb.WriteString(line + "\n")
	}
	return reply{text: strings.TrimRight(sb.String(), "\n")}
}

func (b *bot) purge(m member, g grant, c command) reply {
	if !g.manage {
		return say("Only a manager may purge.")
	}
	if c.user == "" {
		return say("Name the member to purge.")
	}
	es, err := b.chain.list()
	if err != nil {
		log.Printf("purge: %v", err)
		return say(unreachable)
	}
	theirs := withTag(es, tagFor(c.user))
	if len(theirs) == 0 {
		return say("<@%s> has no whitelisted accounts.", c.user)
	}
	removed, lag := b.removeAll(theirs)
	if len(removed) == 0 {
		return say(unreachable)
	}
	return reply{
		text:  fmt.Sprintf("Removed %d account%s of <@%s>: %s.%s", len(removed), plural(len(removed)), c.user, names(removed), lag),
		audit: fmt.Sprintf("<@%s> purged <@%s>: %s", m.id, c.user, names(removed)),
	}
}

// removeAll drops every line given and reports which went.
func (b *bot) removeAll(es []control.Entry) (removed []control.Entry, lag string) {
	for _, e := range es {
		res, err := b.chain.remove(e.UUID)
		if err != nil || !res.rep.OK {
			log.Printf("remove %s: %v %s", e.Name, err, res.rep.Error)
			continue
		}
		removed = append(removed, e)
		if l := res.lag(); l != "" {
			lag = l
		}
	}
	return removed, lag
}

// prune drops the lines of members who left the server or no longer hold a
// configured role. It asks Discord about each member once per pass, which needs
// no privileged intent, and acts only on a definite answer: a member Discord
// could not be asked about keeps their lines until it can. Returns one audit
// line per member acted on.
func (b *bot) prune() []string {
	if b.members == nil {
		return nil
	}
	es, err := b.chain.list()
	if err != nil {
		log.Printf("prune: %v", err)
		return nil
	}
	byOwner := map[string][]control.Entry{}
	for _, e := range es {
		if who := owner(e.Tag); who != "" {
			byOwner[who] = append(byOwner[who], e)
		}
	}
	ids := make([]string, 0, len(byOwner))
	for id := range byOwner {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var audit []string
	for _, id := range ids {
		roles, present, err := b.members.lookup(id)
		if err != nil {
			log.Printf("prune: %s: %v", id, err)
			continue
		}
		reason := ""
		switch {
		case !present:
			reason = "left the server"
		case !b.grantFor(roles).any:
			reason = "no longer holds a whitelist role"
		}
		if reason == "" {
			continue
		}
		removed, _ := b.removeAll(byOwner[id])
		if len(removed) == 0 {
			continue
		}
		audit = append(audit, fmt.Sprintf("removed %d account%s of <@%s>, who %s: %s",
			len(removed), plural(len(removed)), id, reason, names(removed)))
	}
	return audit
}

func withTag(es []control.Entry, tag string) []control.Entry {
	var out []control.Entry
	for _, e := range es {
		if e.Tag == tag {
			out = append(out, e)
		}
	}
	return out
}

func names(es []control.Entry) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = "`" + e.Name + "`"
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// limiter allows n events per window per key. It bounds what one member can
// make the node ask Mojang; nothing else here costs anything.
type limiter struct {
	n      int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.n {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}
