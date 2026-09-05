package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/olgm/proxy/internal/control"
)

// whitelistCmd drives every whitelisted entry's control link over ssh, through
// `proxyd ctl` on the node. An add or remove goes to every entry, so a player is
// whitelisted on the chain rather than on one node; a list shows the entries
// side by side, marking lines that are not on all of them. An entry that fails
// is reported and the others still proceed: the fix is to run the command
// again, or to let the bot's reconcile catch it up.
func whitelistCmd(t *Topology, args []string) error {
	seeds, err := t.whitelistSeeds()
	if err != nil {
		return err
	}
	entries := sortedKeys(seeds)
	if len(entries) == 0 {
		return errors.New("no route has a whitelist")
	}
	if len(args) == 0 {
		usage()
		return errors.New("whitelist needs a command")
	}
	switch args[0] {
	case "list":
		return whitelistList(t, entries)
	case "add":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: whitelist add <name> [uuid]")
		}
		return whitelistEach(t, entries, append(args, "cli"))
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: whitelist remove <name|uuid>")
		}
		return whitelistEach(t, entries, args)
	}
	usage()
	return fmt.Errorf("unknown whitelist command %q", args[0])
}

func whitelistEach(t *Topology, entries []string, args []string) error {
	failed := 0
	for _, name := range entries {
		rep, err := ctl(t.Nodes[name].SSH, args...)
		switch {
		case err != nil:
			fmt.Printf("== %-4s %v\n", name, err)
			failed++
		case rep.Listed:
			fmt.Printf("== %-4s already listed as %s\n", name, line(*rep.Entry))
			failed++
		case !rep.OK:
			fmt.Printf("== %-4s %s\n", name, rep.Error)
			failed++
		default:
			fmt.Printf("== %-4s %s %s\n", name, past(args[0]), line(*rep.Entry))
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d entries did not apply it; run again once they are reachable", failed, len(entries))
	}
	return nil
}

func past(op string) string {
	if op == "add" {
		return "added"
	}
	return "removed"
}

// whitelistList prints one line per player with a column per entry: * where the
// player is listed, - where not, ? where the entry could not be asked.
func whitelistList(t *Topology, entries []string) error {
	lists := map[string]map[string]control.Entry{} // node -> bare uuid -> entry
	var order []string                             // uuids in first-seen order
	seen := map[string]control.Entry{}
	for _, name := range entries {
		rep, err := ctl(t.Nodes[name].SSH, "list")
		if err == nil {
			err = rep.Err()
		}
		if err != nil {
			fmt.Printf("== %-4s %v\n", name, err)
			continue
		}
		lists[name] = map[string]control.Entry{}
		for _, e := range rep.Entries {
			k := bare(e.UUID)
			lists[name][k] = e
			if _, ok := seen[k]; !ok {
				seen[k] = e
				order = append(order, k)
			}
		}
	}
	if len(order) == 0 {
		fmt.Println("(nobody is whitelisted)")
		return nil
	}
	if len(entries) == 1 {
		for _, k := range order {
			fmt.Println(line(seen[k]))
		}
		return nil
	}

	fmt.Println(strings.Join(padAll(entries), " "))
	uneven := 0
	for _, k := range order {
		var cols []string
		everywhere := true
		for _, name := range entries {
			l, asked := lists[name]
			switch {
			case !asked:
				cols = append(cols, "?")
				everywhere = false
			case l[k].UUID != "":
				cols = append(cols, "*")
			default:
				cols = append(cols, "-")
				everywhere = false
			}
		}
		if !everywhere {
			uneven++
		}
		fmt.Printf("%s %s\n", strings.Join(padTo(cols, entries), " "), line(seen[k]))
	}
	if uneven > 0 {
		fmt.Printf("\n%d not on every entry; add again, or let the bot's reconcile settle it\n", uneven)
	}
	return nil
}

func padAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%-4s", n)
	}
	return out
}

func padTo(cols, names []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = fmt.Sprintf("%-*s", len(fmt.Sprintf("%-4s", names[i])), c)
	}
	return out
}

func bare(uuid string) string { return strings.ToLower(strings.ReplaceAll(uuid, "-", "")) }

func line(e control.Entry) string {
	s := e.Name + ":" + e.UUID
	if e.Tag != "" {
		s += " # " + e.Tag
	}
	return s
}

// ctl runs `proxyd ctl` on a node and returns its reply. A refused request exits
// non-zero over there, so the reply is parsed before the exit status is believed.
func ctl(target string, args ...string) (control.Reply, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	out, err := ssh(target, "proxyd ctl -json "+strings.Join(quoted, " "))
	var rep control.Reply
	if jerr := json.Unmarshal(lastLine(out), &rep); jerr == nil {
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	return rep, fmt.Errorf("unexpected reply: %s", strings.TrimSpace(string(out)))
}

func lastLine(b []byte) []byte {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return []byte(lines[len(lines)-1])
}
