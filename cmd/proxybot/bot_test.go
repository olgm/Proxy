package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/mojang"
	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/whitelist"
)

const (
	notchUUID = "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	alexUUID  = "853c80ef-3c37-49fd-aa49-938b674adae6"
	steveUUID = "8667ba71-b85a-4004-af54-457a9734eed7"

	memberRole  = "role-member"
	boosterRole = "role-booster"
	managerRole = "role-manager"
	otherRole   = "role-unrelated"
)

var players = map[string]string{"notch": notchUUID, "alex": alexUUID, "steve": steveUUID}

// fakeSessions stands in for a node's live register and its session log. Tests
// set both directly; the point here is what the bot does with the answers.
type fakeSessions struct {
	mu   sync.Mutex
	live []control.Live
	past []control.Past // newest last, as a log file holds them
}

func (f *fakeSessions) Live() []control.Live {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]control.Live(nil), f.live...)
}

func (f *fakeSessions) History(uuids []string, limit int) ([]control.Past, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]bool{}
	for _, u := range uuids {
		want[bare(u)] = true
	}
	var out []control.Past
	for i := len(f.past) - 1; i >= 0 && len(out) < limit; i-- {
		if len(want) == 0 || want[bare(f.past[i].UUID)] {
			out = append(out, f.past[i])
		}
	}
	return out, nil
}

func (f *fakeSessions) set(live []control.Live, past []control.Past) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live, f.past = live, past
}

type fakeMojang struct{}

func (fakeMojang) LookupName(name string) (string, string, error) {
	u, ok := players[strings.ToLower(name)]
	if !ok {
		return "", "", mojang.ErrNoSuchPlayer
	}
	return strings.ReplaceAll(u, "-", ""), strings.ToUpper(name[:1]) + strings.ToLower(name[1:]), nil
}

func (fakeMojang) LookupUUID(uuid string) (string, error) {
	for n, u := range players {
		if bare(u) == bare(uuid) {
			return strings.ToUpper(n[:1]) + n[1:], nil
		}
	}
	return "", mojang.ErrNoSuchPlayer
}

type fakeNode struct {
	path string
	ln   net.Listener
	sess *fakeSessions
}

func (n *fakeNode) file(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(n.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (n *fakeNode) write(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(n.path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func startNode(t *testing.T, name string) (botcfg.Entry, *fakeNode) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name+".txt")
	if err := os.WriteFile(p, []byte("# "+name+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	l, err := whitelist.Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := tunnel.NewKey()
	sess := &fakeSessions{}
	s, err := control.NewServer(l, key, nil, fakeMojang{}, sess)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.Serve(ln)
	return botcfg.Entry{Node: name, Addr: ln.Addr().String(), Key: tunnel.EncodeKey(key)}, &fakeNode{path: p, ln: ln, sess: sess}
}

type fakeMembers struct {
	roles map[string][]string // present members
	fail  map[string]bool     // Discord could not say
}

func (f *fakeMembers) lookup(id string) ([]string, bool, error) {
	if f.fail[id] {
		return nil, false, errors.New("503")
	}
	r, ok := f.roles[id]
	return r, ok, nil
}

func newBot(t *testing.T) (*bot, *fakeNode, *fakeNode, *fakeMembers) {
	t.Helper()
	hkCfg, hk := startNode(t, "hk")
	tyCfg, ty := startNode(t, "ty")
	cfg := &botcfg.Config{
		Guild: "guild",
		Roles: map[string]botcfg.Role{
			memberRole:  {Accounts: 1},
			boosterRole: {Accounts: 3},
			managerRole: {Manage: true},
		},
		Primary: "hk",
		Entries: []botcfg.Entry{hkCfg, tyCfg},
	}
	ch, err := newChain(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeMembers{roles: map[string][]string{}, fail: map[string]bool{}}
	return &bot{cfg: cfg, chain: ch, adds: newLimiter(5, time.Minute), members: fm}, hk, ty, fm
}

func mem(id string, roles ...string) member { return member{id: id, roles: roles} }

func add(b *bot, m member, player string) reply {
	return b.handle(m, command{op: "add", player: player})
}

func want(t *testing.T, r reply, substr string) {
	t.Helper()
	if !strings.Contains(r.text, substr) {
		t.Fatalf("reply %q, want it to contain %q", r.text, substr)
	}
}

func TestNoRoleIsRefused(t *testing.T) {
	b, hk, _, _ := newBot(t)
	for _, m := range []member{mem("u1"), mem("u1", otherRole)} {
		want(t, add(b, m, "notch"), "no role")
	}
	if hk.file(t) != "# hk\n" {
		t.Fatal("a refused add wrote something")
	}
}

func TestMemberAddsWithinCapOnEveryEntry(t *testing.T) {
	b, hk, ty, _ := newBot(t)
	m := mem("u1", memberRole)

	r := add(b, m, "notch")
	want(t, r, "Added `Notch` (`"+notchUUID+"`)")
	if r.audit == "" || !strings.Contains(r.audit, "<@u1> added `Notch`") {
		t.Fatalf("audit %q", r.audit)
	}
	line := "Notch:" + notchUUID + " # discord:u1\n"
	if hk.file(t) != "# hk\n"+line || ty.file(t) != "# ty\n"+line {
		t.Fatalf("files:\n%s---\n%s", hk.file(t), ty.file(t))
	}

	want(t, add(b, m, "alex"), "You may whitelist 1 account and have 1")
	want(t, add(b, m, "notch"), "already on the whitelist for <@u1>")

	// A booster has room for three; a name nobody holds is said so.
	bo := mem("u2", boosterRole, otherRole)
	want(t, add(b, bo, "alex"), "Added `Alex`")
	want(t, add(b, bo, "notch"), "already whitelisted by <@u1>")
	want(t, add(b, bo, "herobrine"), "no such player")
	want(t, add(b, bo, "not a name"), "not a Minecraft name")
	want(t, add(b, bo, steveUUID), "Added `Steve`")
}

func TestAddForAnotherNeedsManage(t *testing.T) {
	b, hk, _, _ := newBot(t)
	r := b.handle(mem("u1", memberRole), command{op: "add", player: "notch", user: "u2"})
	want(t, r, "Only a manager")

	r = b.handle(mem("mgr", managerRole), command{op: "add", player: "notch", user: "u2"})
	want(t, r, "Added `Notch` (`"+notchUUID+"`) for <@u2>")
	if !strings.Contains(hk.file(t), "# discord:u2") {
		t.Fatalf("line tagged wrongly:\n%s", hk.file(t))
	}
	// A manager's own adds have no cap.
	for _, p := range []string{"alex", "steve"} {
		want(t, add(b, mem("mgr", managerRole), p), "Added")
	}
}

func TestRemoveOwnLinesOnly(t *testing.T) {
	b, hk, ty, _ := newBot(t)
	add(b, mem("u1", memberRole), "notch")
	add(b, mem("u2", memberRole), "alex")

	want(t, b.handle(mem("u1", memberRole), command{op: "remove", player: "alex"}), "was not whitelisted by you")
	want(t, b.handle(mem("u1", memberRole), command{op: "remove", player: "steve"}), "is not whitelisted")
	r := b.handle(mem("u1", memberRole), command{op: "remove", player: "NOTCH"})
	want(t, r, "Removed `Notch`")
	if strings.Contains(hk.file(t), "Notch") || strings.Contains(ty.file(t), "Notch") {
		t.Fatal("Notch survived on an entry")
	}

	r = b.handle(mem("mgr", managerRole), command{op: "remove", player: alexUUID})
	want(t, r, "Removed `Alex`")
	if !strings.Contains(r.audit, "whitelisted by <@u2>") {
		t.Fatalf("audit %q", r.audit)
	}
}

func TestListViews(t *testing.T) {
	b, hk, _, _ := newBot(t)
	add(b, mem("u1", memberRole), "notch")
	add(b, mem("u2", memberRole), "alex")
	hk.write(t, hk.file(t)+"Steve:"+steveUUID+" # cli\n")

	r := b.handle(mem("u1", memberRole), command{op: "list"})
	want(t, r, "Your accounts (1)")
	want(t, r, "`Notch`")
	if strings.Contains(r.text, "Alex") {
		t.Fatalf("a member saw someone else's line: %q", r.text)
	}
	want(t, b.handle(mem("u1", memberRole), command{op: "list", user: "u2"}), "Only a manager")
	want(t, b.handle(mem("u3", memberRole), command{op: "list"}), "Your accounts: none")

	r = b.handle(mem("mgr", managerRole), command{op: "list"})
	want(t, r, "Every whitelisted account (3)")
	want(t, r, "`Alex` `"+alexUUID+"` — <@u2>")
	want(t, r, "`Steve` `"+steveUUID+"` — `cli`")
	r = b.handle(mem("mgr", managerRole), command{op: "list", user: "u2"})
	want(t, r, "<@u2>'s accounts (1)")
}

func TestPurge(t *testing.T) {
	b, hk, ty, _ := newBot(t)
	bo := mem("u1", boosterRole)
	add(b, bo, "notch")
	add(b, bo, "alex")
	add(b, mem("u2", memberRole), "steve")

	want(t, b.handle(bo, command{op: "purge", user: "u2"}), "Only a manager")
	r := b.handle(mem("mgr", managerRole), command{op: "purge", user: "u1"})
	want(t, r, "Removed 2 accounts of <@u1>: `Notch`, `Alex`")
	for _, n := range []*fakeNode{hk, ty} {
		f := n.file(t)
		if strings.Contains(f, "Notch") || strings.Contains(f, "Alex") || !strings.Contains(f, "Steve") {
			t.Fatalf("after purge:\n%s", f)
		}
	}
	want(t, b.handle(mem("mgr", managerRole), command{op: "purge", user: "u1"}), "has no whitelisted accounts")
}

// The secondary is made to match the primary's set of uuid and tag, whatever
// happened to it, and only that: names are each node's own business.
func TestReconcileBringsSecondaryLevel(t *testing.T) {
	b, _, ty, _ := newBot(t)
	add(b, mem("u1", memberRole), "notch")
	add(b, mem("u2", memberRole), "alex")

	add(b, mem("u3", memberRole), "steve")
	ty.write(t, "# ty\n"+
		"Notch:"+notchUUID+" # discord:somebody-else\n"+ // wrong owner
		"AlexOnTy:"+alexUUID+" # discord:u2\n"+ // level, under the name this node knows
		"Stray:11111111-2222-3333-4444-555555555555 # discord:u9\n") // not on the primary; Steve missing

	b.chain.reconcile()

	f := ty.file(t)
	for _, w := range []string{
		"Notch:" + notchUUID + " # discord:u1",   // owner fixed, with the primary's name
		"AlexOnTy:" + alexUUID + " # discord:u2", // level: left alone, name and all
		"Steve:" + steveUUID + " # discord:u3",
	} {
		if !strings.Contains(f, w) {
			t.Errorf("secondary lacks %q:\n%s", w, f)
		}
	}
	if strings.Contains(f, "Stray") || strings.Contains(f, "somebody-else") {
		t.Errorf("secondary kept what the primary does not have:\n%s", f)
	}
	// Idle when level: the file is not rewritten.
	before, _ := os.Stat(ty.path)
	b.chain.reconcile()
	after, _ := os.Stat(ty.path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a level secondary was rewritten")
	}
}

func TestPruneFollowsRoles(t *testing.T) {
	b, hk, ty, fm := newBot(t)
	add(b, mem("stays", memberRole), "notch")
	add(b, mem("left", memberRole), "alex")
	add(b, mem("demoted", memberRole), "steve")
	hk.write(t, hk.file(t)+"Operator:11111111-2222-3333-4444-555555555555 # cli\n")
	fm.roles["stays"] = []string{memberRole, otherRole}
	fm.roles["demoted"] = []string{otherRole}
	// "left" is absent from the guild entirely.

	audit := b.prune()
	if len(audit) != 2 {
		t.Fatalf("audit lines: %q", audit)
	}
	if !strings.Contains(audit[0], "<@demoted>, who no longer holds a whitelist role: `Steve`") ||
		!strings.Contains(audit[1], "<@left>, who left the server: `Alex`") {
		t.Fatalf("audit lines: %q", audit)
	}
	for _, n := range []*fakeNode{hk, ty} {
		f := n.file(t)
		if strings.Contains(f, "Alex") || strings.Contains(f, "Steve") || !strings.Contains(f, "Notch") {
			t.Fatalf("after prune:\n%s", f)
		}
	}
	if !strings.Contains(hk.file(t), "Operator:") {
		t.Fatal("prune touched a line the bot did not write")
	}

	// Discord not answering is not an answer.
	delete(fm.roles, "stays")
	fm.fail["stays"] = true
	if audit := b.prune(); len(audit) != 0 || !strings.Contains(hk.file(t), "Notch") {
		t.Fatalf("pruned on a failed lookup: %q", audit)
	}
}

func TestLaggingEntryIsReportedNotFatal(t *testing.T) {
	b, hk, ty, _ := newBot(t)
	ty.ln.Close()
	r := add(b, mem("u1", memberRole), "notch")
	want(t, r, "Added `Notch`")
	want(t, r, "has not reached ty yet")
	if !strings.Contains(hk.file(t), "Notch") {
		t.Fatal("the primary did not take the add")
	}
	// The primary down is a refusal to the member, with nothing written.
	hk.ln.Close()
	want(t, add(b, mem("u2", memberRole), "alex"), "could not be reached")
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Fatal("two per window, then no")
	}
	if !l.allow("b") {
		t.Fatal("keys are independent")
	}
	now = now.Add(61 * time.Second)
	if !l.allow("a") {
		t.Fatal("the window did not pass")
	}
}

func TestLoadConfigRejectsUseless(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bot.json")
	for _, body := range []string{
		`{}`,
		`{"guild":"g","roles":{},"primary":"hk","entries":[{"node":"hk","addr":"a","key":"k"}]}`,
		`{"guild":"g","roles":{"r":{"accounts":1}},"primary":"hk","entries":[]}`,
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := botcfg.Load(p); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
	// A primary that is not an entry is caught by the chain.
	cfg := &botcfg.Config{Primary: "sg", Entries: []botcfg.Entry{{Node: "hk", Addr: "a", Key: tunnel.EncodeKey(tunnel.NewKey())}}}
	if _, err := newChain(cfg); err == nil {
		t.Error("a primary outside the entries was accepted")
	}
}
