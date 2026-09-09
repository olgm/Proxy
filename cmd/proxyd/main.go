// Command proxyd is the node runtime. Every node in a chain runs this same binary;
// what it does is entirely determined by its config file.
//
// `proxyd ctl` is the local client of the node's control link, for managing the
// whitelist from the node itself; see ctl.go.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

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
	if err := proxy.Run(cfg, stop); err != nil {
		log.Fatalf("%v", err)
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
