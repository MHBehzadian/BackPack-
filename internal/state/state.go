// Package state persists the few decisions BackPack+ must remember across restarts.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type State struct {
	Active     string    `json:"active"`      // server name the record points at
	LastSwitch time.Time `json:"last_switch"` // when BackPack+ last changed the record
	Auto       *bool     `json:"auto,omitempty"`
	Pinned     bool      `json:"pinned"` // manual /switch: no automatic failback until /release
	// Primary is the primary chosen from the bot (/primary). It applies only
	// while the config file still names ConfigPrimary; editing the config's
	// primary wins over an older choice made from the bot.
	Primary       string `json:"primary,omitempty"`
	ConfigPrimary string `json:"config_primary,omitempty"`
}

func Load(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Save writes atomically (temp file + rename).
func Save(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
