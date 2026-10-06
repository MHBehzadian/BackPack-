// Package config loads and validates the BackPack+ configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Duration is a time.Duration that reads "30s", "10m" (or plain seconds) from JSON.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("bad duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds")
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Probe describes how a server's tunnel is checked from the kharej side.
type Probe struct {
	// Type is "echo" (end-to-end through the tunnel, recommended) or "tcp"
	// (plain TCP connect to a forwarded port on the Iran server).
	Type string `json:"type"`
	// Port on the Iran server. For "echo" it must be a forwarded port of that
	// server's tunnel that points back at EchoListen on this kharej server.
	Port int `json:"port"`
	// Host overrides the address probed; defaults to the server's IP.
	Host string `json:"host,omitempty"`
}

// Server is one Iran server, in priority order (the first one is the primary).
type Server struct {
	Name  string `json:"name"`
	IP    string `json:"ip"`
	Probe Probe  `json:"probe"`
}

type Failover struct {
	// Consecutive failed probes after which a server is considered down.
	FailThreshold int `json:"fail_threshold"`
	// Consecutive successful probes after which a down server counts as up again.
	RecoverThreshold int `json:"recover_threshold"`
	// A server that drops (goes from answering to failing) FlapMaxDrops times
	// inside FlapWindow is "unstable", even if no drop lasts FailThreshold probes.
	FlapWindow   Duration `json:"flap_window"`
	FlapMaxDrops int      `json:"flap_max_drops"`
	// A higher-priority server must be clean for this long before traffic returns to it.
	FailbackAfter Duration `json:"failback_after"`
	// Minimum time between two non-emergency switches (unstable / failback).
	MinHold Duration `json:"min_hold"`
	// Switch automatically. Can be toggled at runtime from the bot (/auto).
	Auto *bool `json:"auto,omitempty"`
}

type Arvan struct {
	APIKey  string `json:"api_key"`
	APIBase string `json:"api_base,omitempty"`
	// Domain is the zone as registered in Arvan, e.g. "example.com".
	Domain string `json:"domain"`
	// Record is the record name inside the zone, e.g. "tun" for tun.example.com, "@" for the apex.
	Record string `json:"record"`
	// RecordID is optional; it is looked up from Record when empty.
	RecordID string `json:"record_id,omitempty"`
	// Proxy is an optional HTTP(S) proxy URL used only for the Arvan API.
	Proxy string `json:"proxy,omitempty"`
}

type DNSVerify struct {
	// Resolvers queried after a switch to see when the domain points at the new server.
	Resolvers []string `json:"resolvers"`
	Interval  Duration `json:"interval"`
	Timeout   Duration `json:"timeout"`
}

type Telegram struct {
	BotToken string  `json:"bot_token"`
	ChatIDs  []int64 `json:"chat_ids"`
	APIBase  string  `json:"api_base,omitempty"`
	Proxy    string  `json:"proxy,omitempty"`
	// Label is prefixed to every message, handy when several BackPack+ instances share a chat.
	Label string `json:"label,omitempty"`
}

type Config struct {
	CheckInterval Duration  `json:"check_interval"`
	ProbeTimeout  Duration  `json:"probe_timeout"`
	EchoListen    string    `json:"echo_listen"`
	StateFile     string    `json:"state_file"`
	Servers       []Server  `json:"servers"`
	Failover      Failover  `json:"failover"`
	Arvan         Arvan     `json:"arvan"`
	DNSVerify     DNSVerify `json:"dns_verify"`
	Telegram      Telegram  `json:"telegram"`
}

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.CheckInterval == 0 {
		c.CheckInterval = Duration(10 * time.Second)
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = Duration(5 * time.Second)
	}
	if c.EchoListen == "" {
		c.EchoListen = "127.0.0.1:59998"
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/backpack-plus/state.json"
	}
	for i := range c.Servers {
		if c.Servers[i].Probe.Type == "" {
			c.Servers[i].Probe.Type = "echo"
		}
	}
	f := &c.Failover
	if f.FailThreshold == 0 {
		f.FailThreshold = 3
	}
	if f.RecoverThreshold == 0 {
		f.RecoverThreshold = 3
	}
	if f.FlapWindow == 0 {
		f.FlapWindow = Duration(5 * time.Minute)
	}
	if f.FlapMaxDrops == 0 {
		f.FlapMaxDrops = 3
	}
	if f.FailbackAfter == 0 {
		f.FailbackAfter = Duration(10 * time.Minute)
	}
	if f.MinHold == 0 {
		f.MinHold = Duration(3 * time.Minute)
	}
	if f.Auto == nil {
		t := true
		f.Auto = &t
	}
	if c.Arvan.APIBase == "" {
		c.Arvan.APIBase = "https://napi.arvancloud.ir/cdn/4.0"
	}
	if c.Arvan.Record == "" {
		c.Arvan.Record = "@"
	}
	if len(c.DNSVerify.Resolvers) == 0 {
		c.DNSVerify.Resolvers = []string{"8.8.8.8:53", "1.1.1.1:53", "9.9.9.9:53"}
	}
	for i, r := range c.DNSVerify.Resolvers {
		if _, _, err := net.SplitHostPort(r); err != nil {
			c.DNSVerify.Resolvers[i] = net.JoinHostPort(r, "53")
		}
	}
	if c.DNSVerify.Interval == 0 {
		c.DNSVerify.Interval = Duration(20 * time.Second)
	}
	if c.DNSVerify.Timeout == 0 {
		c.DNSVerify.Timeout = Duration(20 * time.Minute)
	}
	if c.Telegram.APIBase == "" {
		c.Telegram.APIBase = "https://api.telegram.org"
	}
}

func (c *Config) Validate() error {
	var errs []error
	if len(c.Servers) < 2 {
		errs = append(errs, errors.New("at least two servers are required"))
	}
	names := map[string]bool{}
	ips := map[string]bool{}
	for i, s := range c.Servers {
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("servers[%d]: name is required", i))
		}
		if names[s.Name] {
			errs = append(errs, fmt.Errorf("servers[%d]: duplicate name %q", i, s.Name))
		}
		names[s.Name] = true
		if net.ParseIP(s.IP) == nil {
			errs = append(errs, fmt.Errorf("servers[%d] %s: ip %q is not an IP address", i, s.Name, s.IP))
		}
		if ips[s.IP] {
			errs = append(errs, fmt.Errorf("servers[%d] %s: duplicate ip %s", i, s.Name, s.IP))
		}
		ips[s.IP] = true
		switch s.Probe.Type {
		case "echo", "tcp":
		default:
			errs = append(errs, fmt.Errorf("servers[%d] %s: probe.type must be \"echo\" or \"tcp\"", i, s.Name))
		}
		if s.Probe.Port <= 0 || s.Probe.Port > 65535 {
			errs = append(errs, fmt.Errorf("servers[%d] %s: probe.port is required", i, s.Name))
		}
	}
	if c.CheckInterval.D() < time.Second {
		errs = append(errs, errors.New("check_interval must be at least 1s"))
	}
	if c.ProbeTimeout.D() >= c.CheckInterval.D() {
		errs = append(errs, errors.New("probe_timeout must be shorter than check_interval"))
	}
	if c.Failover.FailThreshold < 1 || c.Failover.RecoverThreshold < 1 || c.Failover.FlapMaxDrops < 1 {
		errs = append(errs, errors.New("failover thresholds must be positive"))
	}
	if c.Arvan.APIKey == "" {
		errs = append(errs, errors.New("arvan.api_key is required"))
	}
	if c.Arvan.Domain == "" {
		errs = append(errs, errors.New("arvan.domain is required"))
	}
	if c.Telegram.BotToken != "" && len(c.Telegram.ChatIDs) == 0 {
		errs = append(errs, errors.New("telegram.chat_ids is required when bot_token is set"))
	}
	return errors.Join(errs...)
}

// FQDN is the full name of the managed record, e.g. tun.example.com.
func (c *Config) FQDN() string {
	r := strings.TrimSpace(c.Arvan.Record)
	if r == "" || r == "@" {
		return c.Arvan.Domain
	}
	return r + "." + c.Arvan.Domain
}

func (c *Config) ServerByIP(ip string) (int, bool) {
	for i, s := range c.Servers {
		if s.IP == ip {
			return i, true
		}
	}
	return -1, false
}

func (c *Config) ServerByName(name string) (int, bool) {
	for i, s := range c.Servers {
		if strings.EqualFold(s.Name, name) {
			return i, true
		}
	}
	return -1, false
}
