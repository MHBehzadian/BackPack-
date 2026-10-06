package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mhbehzadian/backpack-plus/internal/arvan"
	"github.com/mhbehzadian/backpack-plus/internal/config"
	"github.com/mhbehzadian/backpack-plus/internal/telegram"
)

// wizard asks questions on in and writes prompts to out.
type wizard struct {
	in  *bufio.Reader
	out io.Writer
}

var errAborted = errors.New("setup aborted (input closed)")

func (w *wizard) ask(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(w.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(w.out, "%s: ", prompt)
	}
	line, err := w.in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		fmt.Fprintln(w.out)
		return "", errAborted
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// askValid repeats the question until check accepts the answer.
func (w *wizard) askValid(prompt, def string, check func(string) error) (string, error) {
	for {
		v, err := w.ask(prompt, def)
		if err != nil {
			return "", err
		}
		if err := check(v); err != nil {
			fmt.Fprintf(w.out, "  ✗ %v\n", err)
			continue
		}
		return v, nil
	}
}

func (w *wizard) askInt(prompt string, def, min, max int) (int, error) {
	v, err := w.askValid(prompt, strconv.Itoa(def), func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < min || n > max {
			return fmt.Errorf("enter a number from %d to %d", min, max)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(v)
	return n, nil
}

func (w *wizard) yes(prompt string, def bool) (bool, error) {
	d := "y"
	if !def {
		d = "n"
	}
	v, err := w.askValid(prompt+" (y/n)", d, func(s string) error {
		switch strings.ToLower(s) {
		case "y", "yes", "n", "no":
			return nil
		}
		return errors.New("answer y or n")
	})
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToLower(v), "y"), nil
}

// parseTunnels reads "59999:main,59997:panel" (names optional).
func parseTunnels(s string) ([]config.Tunnel, error) {
	var out []config.Tunnel
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		portStr, name, _ := strings.Cut(part, ":")
		port, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%q: expected port or port:name, e.g. 59999:main", part)
		}
		if seen[port] {
			return nil, fmt.Errorf("port %d is listed twice", port)
		}
		seen[port] = true
		name = strings.TrimSpace(name)
		if strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("%q: tunnel names cannot contain spaces", name)
		}
		out = append(out, config.Tunnel{Name: name, Port: port, Type: "echo"})
	}
	if len(out) == 0 {
		return nil, errors.New("at least one probe port is needed")
	}
	return out, nil
}

func formatTunnels(ts []config.Tunnel) string {
	var parts []string
	for _, t := range ts {
		if t.Name == "" || t.Name == fmt.Sprintf("port %d", t.Port) {
			parts = append(parts, strconv.Itoa(t.Port))
		} else {
			parts = append(parts, fmt.Sprintf("%d:%s", t.Port, t.Name))
		}
	}
	return strings.Join(parts, ",")
}

func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 6) + s[len(s)-4:]
}

// loadLoose reads an existing config for its values without validating it,
// so a half-finished file still provides the defaults.
func loadLoose(path string) *config.Config {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if c, err := config.Parse(b); err == nil {
		return c
	}
	var c config.Config
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

func setup(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	w := &wizard{in: bufio.NewReader(in), out: out}
	old := loadLoose(path)
	cfg := &config.Config{}
	if old != nil {
		*cfg = *old
		fmt.Fprintf(out, "Editing %s — press Enter to keep the value in [brackets].\n\n", path)
	} else {
		fmt.Fprintf(out, "Creating %s — press Enter to accept the value in [brackets].\n\n", path)
	}

	// --- Iran servers
	fmt.Fprintln(out, "== Iran servers ==")
	fmt.Fprintln(out, "For each Iran server: the IP the domain should point at, and the probe port of")
	fmt.Fprintln(out, "every tunnel it has to this server. iran-setup.sh prints these ports for you.")
	n, err := w.askInt("How many Iran servers", max(2, len(cfg.Servers)), 2, 20)
	if err != nil {
		return err
	}
	servers := make([]config.Server, n)
	names := map[string]bool{}
	ips := map[string]bool{}
	for i := range servers {
		var prev config.Server
		if i < len(cfg.Servers) {
			prev = cfg.Servers[i]
		}
		fmt.Fprintf(out, "\n-- server %d --\n", i+1)
		defName := prev.Name
		if defName == "" {
			defName = fmt.Sprintf("iran%d", i+1)
		}
		name, err := w.askValid("Name (used in messages and bot commands)", defName, func(s string) error {
			if strings.ContainsAny(s, " \t/") {
				return errors.New("no spaces or slashes")
			}
			if names[strings.ToLower(s)] {
				return errors.New("this name is already used")
			}
			return nil
		})
		if err != nil {
			return err
		}
		names[strings.ToLower(name)] = true
		ip, err := w.askValid("Public IP (what the DNS record should hold)", prev.IP, func(s string) error {
			if ip := net.ParseIP(s); ip == nil || ip.To4() == nil {
				return errors.New("enter an IPv4 address")
			}
			if ips[s] {
				return errors.New("this IP is already used by another server")
			}
			return nil
		})
		if err != nil {
			return err
		}
		ips[ip] = true
		defT := formatTunnels(prev.Tunnels)
		if defT == "" {
			defT = "59999"
		}
		var tunnels []config.Tunnel
		_, err = w.askValid("Probe ports of its tunnels, port:name comma separated", defT, func(s string) error {
			var err error
			tunnels, err = parseTunnels(s)
			return err
		})
		if err != nil {
			return err
		}
		servers[i] = config.Server{Name: name, IP: ip, Tunnels: tunnels}
	}
	cfg.Servers = servers

	// --- primary
	fmt.Fprintln(out, "\n== Primary server ==")
	fmt.Fprintln(out, "The server the domain normally points at. After a failover the record returns")
	fmt.Fprintln(out, "to it once it has been stable long enough. It can be changed later from the bot.")
	defPrimary := "1"
	for i, s := range servers {
		fmt.Fprintf(out, "  %d) %s  %s\n", i+1, s.Name, s.IP)
		if old != nil && strings.EqualFold(s.Name, old.Primary) {
			defPrimary = strconv.Itoa(i + 1)
		}
	}
	pv, err := w.askValid("Primary server (number or name)", defPrimary, func(s string) error {
		if _, ok := pickServer(servers, s); !ok {
			return errors.New("pick one of the servers above")
		}
		return nil
	})
	if err != nil {
		return err
	}
	pi, _ := pickServer(servers, pv)
	cfg.Primary = servers[pi].Name

	// --- Arvan
	fmt.Fprintln(out, "\n== ArvanCloud DNS ==")
	for {
		keyDef := ""
		if cfg.Arvan.APIKey != "" {
			keyDef = mask(cfg.Arvan.APIKey)
		}
		key, err := w.askValid("API key (Arvan panel → API keys; needs DNS access)", keyDef, func(s string) error {
			if s == "" {
				return errors.New("required")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if key != keyDef {
			cfg.Arvan.APIKey = key
		}
		domain, err := w.askValid("Domain as registered in Arvan, e.g. example.com", cfg.Arvan.Domain, func(s string) error {
			if s == "" || strings.Contains(s, "://") || !strings.Contains(s, ".") {
				return errors.New("enter the bare domain, e.g. example.com")
			}
			return nil
		})
		if err != nil {
			return err
		}
		cfg.Arvan.Domain = strings.ToLower(strings.TrimSuffix(domain, "."))
		defRec := cfg.Arvan.Record
		if defRec == "" {
			defRec = "@"
		}
		rec, err := w.ask("Record name (tun for tun."+cfg.Arvan.Domain+", @ for the domain itself)", defRec)
		if err != nil {
			return err
		}
		rec = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rec)), ".")
		if rec == cfg.Arvan.Domain || rec == "" {
			rec = "@"
		}
		rec = strings.TrimSuffix(rec, "."+cfg.Arvan.Domain)
		cfg.Arvan.Record = rec
		cfg.Arvan.RecordID = ""

		fmt.Fprintf(out, "  checking %s at Arvan ... ", cfg.FQDN())
		base := cfg.Arvan.APIBase
		if base == "" {
			base = "https://napi.arvancloud.ir/cdn/4.0"
		}
		ac, err := arvan.New(base, cfg.Arvan.APIKey, cfg.Arvan.Domain, cfg.Arvan.Proxy)
		if err == nil {
			actx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var r arvan.Record
			r, err = ac.FindA(actx, cfg.Arvan.Record)
			cancel()
			if err == nil {
				who := "not one of the servers above — BackPack+ will move it to the primary"
				for _, s := range servers {
					if len(r.IPs) == 1 && r.IPs[0] == s.IP {
						who = "= " + s.Name
					}
				}
				fmt.Fprintf(out, "OK\n  %s → %s (%s), TTL %ds, CDN %v\n", cfg.FQDN(), strings.Join(r.IPs, ", "), who, r.TTL, r.Cloud)
				if !r.Cloud && r.TTL > 300 {
					fmt.Fprintf(out, "  ! TTL is %ds: after a switch some users keep the old IP that long. 120s is recommended.\n", r.TTL)
				}
				break
			}
		}
		fmt.Fprintf(out, "FAILED\n  %v\n", err)
		again, err2 := w.yes("Enter the Arvan settings again", true)
		if err2 != nil {
			return err2
		}
		if !again {
			fmt.Fprintln(out, "  keeping them; `backpack-plus check` will test again.")
			break
		}
	}

	// --- Telegram
	fmt.Fprintln(out, "\n== Telegram bot ==")
	fmt.Fprintln(out, "Use a NEW bot from @BotFather, not the one BackPack itself uses (two programs")
	fmt.Fprintln(out, "reading one bot lose each other's commands). Your numeric id: @userinfobot.")
	for {
		tokDef := ""
		if cfg.Telegram.BotToken != "" {
			tokDef = mask(cfg.Telegram.BotToken)
		}
		tok, err := w.ask("Bot token (Enter with nothing to skip Telegram)", tokDef)
		if err != nil {
			return err
		}
		if tok == "" {
			cfg.Telegram.BotToken, cfg.Telegram.ChatIDs = "", nil
			fmt.Fprintln(out, "  Telegram off: events go to the log only (journalctl -u backpack-plus).")
			break
		}
		if tok != tokDef {
			cfg.Telegram.BotToken = tok
		}
		var idsDef []string
		for _, id := range cfg.Telegram.ChatIDs {
			idsDef = append(idsDef, strconv.FormatInt(id, 10))
		}
		var ids []int64
		_, err = w.askValid("Chat id(s) that get messages and may give commands, comma separated", strings.Join(idsDef, ","), func(s string) error {
			ids = nil
			for _, p := range strings.Split(s, ",") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				id, err := strconv.ParseInt(p, 10, 64)
				if err != nil {
					return fmt.Errorf("%q is not a numeric id", p)
				}
				ids = append(ids, id)
			}
			if len(ids) == 0 {
				return errors.New("at least one id")
			}
			return nil
		})
		if err != nil {
			return err
		}
		cfg.Telegram.ChatIDs = ids
		label, err := w.ask("Label in front of every message (optional, '-' for none)", cfg.Telegram.Label)
		if err != nil {
			return err
		}
		if label == "-" {
			label = ""
		}
		cfg.Telegram.Label = label

		fmt.Fprint(out, "  sending a test message ... ")
		apiBase := cfg.Telegram.APIBase
		if apiBase == "" {
			apiBase = "https://api.telegram.org"
		}
		bot, err := telegram.New(apiBase, cfg.Telegram.BotToken, ids, label, cfg.Telegram.Proxy)
		if err == nil {
			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			for _, id := range ids {
				if err = bot.Reply(tctx, id, "🧪 BackPack+ setup: ربات وصل است."); err != nil {
					err = fmt.Errorf("chat %d: %w (did you press Start in the bot first?)", id, err)
					break
				}
			}
			cancel()
		}
		if err == nil {
			fmt.Fprintln(out, "OK — check Telegram")
			break
		}
		fmt.Fprintf(out, "FAILED\n  %v\n", err)
		again, err2 := w.yes("Enter the Telegram settings again", true)
		if err2 != nil {
			return err2
		}
		if !again {
			break
		}
	}

	// --- timing
	fmt.Fprintln(out, "\n== Timing ==")
	fb := 10
	if cfg.Failover.FailbackAfter != 0 {
		fb = int(cfg.Failover.FailbackAfter.D().Minutes())
	}
	fb, err = w.askInt("Minutes the primary must be stable before the domain returns to it", fb, 1, 1440)
	if err != nil {
		return err
	}
	cfg.Failover.FailbackAfter = config.Duration(time.Duration(fb) * time.Minute)
	ci := 10
	if cfg.CheckInterval != 0 {
		ci = int(cfg.CheckInterval.D().Seconds())
	}
	ci, err = w.askInt("Seconds between checks", ci, 3, 300)
	if err != nil {
		return err
	}
	cfg.CheckInterval = config.Duration(time.Duration(ci) * time.Second)
	if cfg.ProbeTimeout.D() >= cfg.CheckInterval.D() || cfg.ProbeTimeout == 0 {
		cfg.ProbeTimeout = config.Duration(min(5*time.Second, cfg.CheckInterval.D()/2))
	}

	// --- write (with every default spelled out, so the file says what runs)
	cfg.ApplyDefaults()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if _, err := config.Parse(b); err != nil {
		return fmt.Errorf("the new configuration is not valid, nothing written: %w", err)
	}
	if err := writeConfig(path, append(b, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ saved %s\n", path)
	fmt.Fprintf(out, "  primary: %s\n", cfg.Primary)
	for _, s := range cfg.Servers {
		fmt.Fprintf(out, "  %-10s %-15s probe ports %s\n", s.Name, s.IP, formatTunnels(s.Tunnels))
	}
	return nil
}

func pickServer(servers []config.Server, s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(servers) {
		return n - 1, true
	}
	for i, sv := range servers {
		if strings.EqualFold(sv.Name, s) {
			return i, true
		}
	}
	return -1, false
}

// writeConfig keeps the previous file as .bak and replaces it atomically.
func writeConfig(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
