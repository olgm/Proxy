// Package botcfg is the shape of /etc/proxyd/bot.json: what proxyctl writes from
// the topology's discord block, and what proxybot reads on the node it runs on.
package botcfg

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Guild string `json:"guild"`
	// Roles maps a Discord role id to what it grants. A member with none of
	// them may not use the bot at all.
	Roles map[string]Role `json:"roles"`
	// AuditChannel, when set, gets one line per change: who did what.
	AuditChannel string `json:"audit_channel,omitempty"`
	// Primary is the entry that answers first and that the others are kept
	// level with.
	Primary string  `json:"primary"`
	Entries []Entry `json:"entries"`
}

// Entry is one whitelisted entry node's control link, with the key it holds.
type Entry struct {
	Node string `json:"node"`
	Addr string `json:"addr"`
	Key  string `json:"key"`
}

// Role is what a Discord role grants. Accounts is how many a member holding it
// may whitelist; Manage lets them act on anyone's lines, with no cap on their
// own. A member with several roles gets the highest cap, and manage if any of
// them says so.
type Role struct {
	Accounts int  `json:"accounts,omitempty"`
	Manage   bool `json:"manage,omitempty"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
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
