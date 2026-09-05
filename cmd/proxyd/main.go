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

	"github.com/olgm/proxy/internal/proxy"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ctl" {
		ctl(os.Args[2:])
		return
	}
	path := flag.String("c", "/etc/proxyd/config.json", "config file")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := readConfig(*path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if err := proxy.Run(cfg); err != nil {
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
