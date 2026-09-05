// Command proxybot is the Discord bot. It runs on one node and reaches every
// entry's control link, so a member's whitelist entry lands on the whole chain.
// Members manage their own accounts within what their roles allow; managers
// manage anyone's. See agents/control-plane.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

// config is /etc/proxyd/bot.json, written by proxyctl from the topology's
// discord block.
type config struct {
	Guild string `json:"guild"`
	// Roles maps a Discord role id to what it grants. A member with none of
	// them may not use the bot at all.
	Roles map[string]Role `json:"roles"`
	// AuditChannel, when set, gets one line per change: who did what.
	AuditChannel string `json:"audit_channel,omitempty"`
	// Primary is the entry that answers first and that the others are kept
	// level with.
	Primary string        `json:"primary"`
	Entries []entryConfig `json:"entries"`
}

type entryConfig struct {
	Node string `json:"node"`
	Addr string `json:"addr"`
	Key  string `json:"key"`
}

func loadConfig(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Guild == "" {
		return nil, fmt.Errorf("%s: guild is required", path)
	}
	if len(c.Roles) == 0 {
		return nil, fmt.Errorf("%s: no roles: nobody could use the bot", path)
	}
	if len(c.Entries) == 0 {
		return nil, fmt.Errorf("%s: no entries to manage", path)
	}
	return &c, nil
}

func main() {
	path := flag.String("c", "/etc/proxyd/bot.json", "bot config")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig(*path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	// The token comes through the environment, from a root-only file the unit
	// names, so it is never on the command line or in a file this process can
	// read back.
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is not set")
	}
	ch, err := newChain(cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	b := &bot{cfg: cfg, chain: ch, adds: newLimiter(5, time.Minute)}
	if err := run(b, token); err != nil {
		log.Fatalf("%v", err)
	}
}
