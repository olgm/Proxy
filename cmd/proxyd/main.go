// Command proxyd is the node runtime. Every node in a chain runs this same binary;
// what it does is entirely determined by its config file.
//
// `proxyd ctl` is the local client of the node's control link, for managing the
// whitelist from the node itself; see ctl.go.
//
// SIGTERM ends every session and stops, which is what `systemctl stop` and
// `restart` do. SIGUSR2 hands every session to the process systemd starts next and
// exits without ending any: that is how a deploy replaces the binary under
// players who are still playing. See internal/handoff and internal/proxy.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/olgm/proxy/internal/handoff"
	"github.com/olgm/proxy/internal/proxy"
	"github.com/olgm/proxy/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ctl" {
		ctl(os.Args[2:])
		return
	}
	path := flag.String("c", "/etc/proxyd/config.json", "config file")
	ver := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *ver {
		fmt.Println(version.String())
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	// First, before the config is even read: a node that fails to start still has
	// to be able to say which build failed.
	log.Printf("proxyd %s", version.String())

	// Whatever the last process handed on. It stays in systemd's store until
	// this one is up, so a start that fails here is retried from the same place.
	from, err := handoff.Take()
	if err != nil {
		log.Printf("%v; starting clean", err)
	}
	if from != nil {
		log.Printf("handoff: took %d descriptor(s) from the last process", len(from.Names()))
	}

	cfg, err := readConfig(*path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	// A deploy is a systemctl restart, so SIGTERM is the ordinary way this
	// process ends and the sessions it is carrying have to survive it being
	// asked. Without this they were killed mid-relay: no logout line, no record,
	// nothing in the feed but a join that never leaves.
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		log.Printf("%v: ending sessions", <-sig)
		close(stop)
	}()

	h := &proxy.Handoff{From: from, Ready: func() { ready(from) }, Drop: func(names []string) {
		if err := handoff.Forget(names); err != nil {
			log.Printf("handoff: forget: %v", err)
		}
	}}
	hand := make(chan struct{})
	// Caught whether or not a handoff is possible: left to its default, SIGUSR2
	// kills the process, and every session with it, without a word.
	usr := make(chan os.Signal, 1)
	signal.Notify(usr, syscall.SIGUSR2)
	go func() {
		for range usr {
			if !handoff.Available() {
				log.Printf("SIGUSR2: no fd store to hand off to; carrying on (FileDescriptorStoreMax unset?)")
				continue
			}
			close(hand)
			return
		}
	}()
	h.Signal, h.Keep = hand, handoff.Store
	if err := proxy.Run(cfg, stop, h); err != nil {
		log.Fatalf("%v", err)
	}
}

// ready tells systemd the node is up, and lets go of what the last process left:
// every descriptor in the store has a copy here now, and a copy left there would
// keep a connection open after this process closed it.
func ready(from *handoff.Inherited) {
	status := "running"
	if handoff.Available() {
		// What proxyctl looks for before it sends SIGUSR2 rather than restarting.
		status = "handoff ready"
	}
	if err := handoff.Ready(status); err != nil {
		log.Printf("notify: %v", err)
	}
	if from != nil {
		if err := handoff.Forget(from.Names()); err != nil {
			log.Printf("handoff: forget: %v", err)
		}
	}
}

func readConfig(path string) (*proxy.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
