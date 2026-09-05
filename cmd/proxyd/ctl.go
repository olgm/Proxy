package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/tunnel"
)

// ctl is the local client of the control link, for an operator on the node;
// proxyctl runs it over ssh. It takes the key from the node's own config, so it
// has to run as root or as a member of the proxyd group.
func ctl(args []string) {
	fs := flag.NewFlagSet("proxyd ctl", flag.ExitOnError)
	path := fs.String("c", "/etc/proxyd/config.json", "config file")
	asJSON := fs.Bool("json", false, "print the reply as JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: proxyd ctl [-c config] [-json] <command>

  list                        every player on this node's whitelist
  add <name> [uuid] [tag...]  list a player; a missing uuid is resolved against Mojang
  add <uuid> [tag...]         the same, resolving the name instead
  remove <name|uuid>          drop a player
`)
	}
	fs.Parse(args)
	if fs.NArg() == 0 {
		fs.Usage()
		os.Exit(2)
	}

	cfg, err := readConfig(*path)
	if err != nil {
		die(err)
	}
	if cfg.Control == nil {
		die(fmt.Errorf("%s: this node has no control link", *path))
	}
	key, err := tunnel.DecodeKey(cfg.Control.Key)
	if err != nil {
		die(err)
	}
	c := &control.Client{Addr: loopback(cfg.Control.Bind), Key: key}

	var rep control.Reply
	switch fs.Arg(0) {
	case "list":
		rep, err = c.Do(control.Request{Op: "list"})
	case "add":
		if fs.NArg() < 2 {
			fs.Usage()
			os.Exit(2)
		}
		who, rest := fs.Arg(1), fs.Args()[2:]
		name, uuid := who, ""
		if control.IsUUID(who) {
			name, uuid = "", who
		} else if len(rest) > 0 && control.IsUUID(rest[0]) {
			uuid, rest = rest[0], rest[1:]
		}
		rep, err = c.Add(name, uuid, strings.Join(rest, " "))
	case "remove":
		if fs.NArg() < 2 {
			fs.Usage()
			os.Exit(2)
		}
		var es []control.Entry
		if es, err = c.List(); err == nil {
			if e, ok := control.Find(es, fs.Arg(1)); ok {
				rep, err = c.Remove(e.UUID)
			} else {
				rep = control.Reply{Error: "not listed"}
			}
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		die(err)
	}

	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(rep)
	} else {
		printReply(fs.Arg(0), rep)
	}
	if !rep.OK {
		os.Exit(1)
	}
}

func printReply(op string, rep control.Reply) {
	switch {
	case rep.Listed:
		fmt.Fprintf(os.Stderr, "proxyd ctl: already listed as %s\n", line(*rep.Entry))
	case !rep.OK:
		fmt.Fprintf(os.Stderr, "proxyd ctl: %s\n", rep.Error)
	case op == "list":
		for _, e := range rep.Entries {
			fmt.Println(line(e))
		}
	case op == "add":
		fmt.Println("added", line(*rep.Entry))
	case op == "remove":
		fmt.Println("removed", line(*rep.Entry))
	}
}

func line(e control.Entry) string {
	s := e.Name + ":" + e.UUID
	if e.Tag != "" {
		s += " # " + e.Tag
	}
	return s
}

// loopback is where to reach a listener bound on this host.
func loopback(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "proxyd ctl:", err)
	os.Exit(1)
}
