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

// Tunnel is one BackPack tunnel between an Iran server and this kharej
// server, checked through its own probe port.
type Tunnel struct {
	// Name is only used in messages, e.g. the BackPack tunnel name.
	Name string `json:"name"`
	// Port on the Iran server. For "echo" it must be a forwarded port of this
	// tunnel that points back at EchoListen on this kharej server.
	Port int `json:"port"`
	// Type is "echo" (end-to-end through the tunnel, recommended) or "tcp"
	// (plain TCP connect to a forwarded port on the Iran server).
	Type string `json:"type,omitempty"`
	// Host overrides the address probed; defaults to the server's IP.
	Host string `json:"host,omitempty"`
}

// Probe is the older single-tunnel form of a server's check, still accepted.
type Probe struct {
	Type string `json:"type"`
	Port int    `json:"port"`
	Host string `json:"host,omitempty"`
}

// Server is one Iran server.
type Server struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	// Tunnels are the BackPack tunnels this server carries to this kharej
	// server; the server is healthy only when every one of them answers
	// (see Failover.RequireAllTunnels).
	Tunnels []Tunnel `json:"tunnels,omitempty"`
	Probe   *Probe   `json:"probe,omitempty"`
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
	// RequireAllTunnels: a server with several tunnels counts as failed when
	// any one of them fails (default). false: only when all of them fail.
	RequireAllTunnels *bool `json:"require_all_tunnels,omitempty"`
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
	CheckInterval Duration `json:"check_interval"`
	ProbeTimeout  Duration `json:"probe_timeout"`
	EchoListen    string   `json:"echo_listen"`
	StateFile     string   `json:"state_file"`
	// Primary is the server the domain should normally point at. The record
	// returns to it after a failover once it has been stable for
	// Failover.FailbackAfter. Defaults to the first server.
	Primary   string    `json:"primary,omitempty"`
	Servers   []Server  `json:"servers"`
	Failover  Failover  `json:"failover"`
	Arvan     Arvan     `json:"arvan"`
	DNSVerify DNSVerify `json:"dns_verify"`
	Telegram  Telegram  `json:"telegram"`
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
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

// ApplyDefaults fills every unset field with its default.
func (c *Config) ApplyDefaults() {
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
		s := &c.Servers[i]
		if len(s.Tunnels) == 0 && s.Probe != nil {
			s.Tunnels = []Tunnel{{Name: "tunnel", Port: s.Probe.Port, Type: s.Probe.Type, Host: s.Probe.Host}}
		}
		s.Probe = nil
		for j := range s.Tunnels {
			t := &s.Tunnels[j]
			if t.Type == "" {
				t.Type = "echo"
			}
			if t.Name == "" {
				t.Name = fmt.Sprintf("port %d", t.Port)
			}
		}
	}
	if c.Primary == "" && len(c.Servers) > 0 {
		c.Primary = c.Servers[0].Name
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
	if f.RequireAllTunnels == nil {
		t := true
		f.RequireAllTunnels = &t
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
		if len(s.Tunnels) == 0 {
			errs = append(errs, fmt.Errorf("servers[%d] %s: at least one tunnel (probe port) is required", i, s.Name))
		}
		ports := map[string]bool{}
		for j, t := range s.Tunnels {
			switch t.Type {
			case "echo", "tcp":
			default:
				errs = append(errs, fmt.Errorf("servers[%d] %s tunnels[%d]: type must be \"echo\" or \"tcp\"", i, s.Name, j))
			}
			if t.Port <= 0 || t.Port > 65535 {
				errs = append(errs, fmt.Errorf("servers[%d] %s tunnels[%d]: port must be 1-65535", i, s.Name, j))
			}
			key := t.Host + ":" + fmt.Sprint(t.Port)
			if ports[key] {
				errs = append(errs, fmt.Errorf("servers[%d] %s: probe port %d is listed twice; every tunnel needs its own port", i, s.Name, t.Port))
			}
			ports[key] = true
		}
	}
	if _, ok := c.ServerByName(c.Primary); !ok && len(c.Servers) > 0 {
		errs = append(errs, fmt.Errorf("primary %q is not one of the servers", c.Primary))
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
