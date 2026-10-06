// Command backpack-plus runs on the kharej server and keeps a domain pointed at
// whichever Iran BackPack server currently has a healthy tunnel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mhbehzadian/backpack-plus/internal/arvan"
	"github.com/mhbehzadian/backpack-plus/internal/config"
	"github.com/mhbehzadian/backpack-plus/internal/controller"
	"github.com/mhbehzadian/backpack-plus/internal/dnscheck"
	"github.com/mhbehzadian/backpack-plus/internal/probe"
	"github.com/mhbehzadian/backpack-plus/internal/state"
	"github.com/mhbehzadian/backpack-plus/internal/telegram"
)

var version = "dev"

const usage = `BackPack+ — automatic Iran-server failover for BackPack tunnels

usage:
  backpack-plus [-config FILE] run      run the monitor (default)
  backpack-plus [-config FILE] check    validate config, read the record, probe every server, send a test message
  backpack-plus version
`

func main() {
	cfgPath := flag.String("config", "/etc/backpack-plus/config.json", "config file")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	log.SetFlags(0)

	cmd := flag.Arg(0)
	if cmd == "version" {
		fmt.Println("backpack-plus", version)
		return
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "", "run":
		err = run(ctx, cfg)
	case "check":
		err = check(ctx, cfg)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// arvanDNS adapts the Arvan client to controller.DNS, remembering the last
// record read so an update can preserve its settings.
type arvanDNS struct {
	c    *arvan.Client
	name string
	id   string
	cur  arvan.Record
}

func (a *arvanDNS) Fetch(ctx context.Context) (controller.RecordInfo, error) {
	var r arvan.Record
	var err error
	if a.id != "" {
		r, err = a.c.Get(ctx, a.id)
	} else {
		r, err = a.c.FindA(ctx, a.name)
	}
	if err != nil {
		return controller.RecordInfo{}, err
	}
	if r.Type != "a" {
		return controller.RecordInfo{}, fmt.Errorf("record %s is type %q, expected an A record", r.Name, r.Type)
	}
	a.id, a.cur = r.ID, r
	return info(r), nil
}

func (a *arvanDNS) Set(ctx context.Context, ip string) (controller.RecordInfo, error) {
	if a.cur.ID == "" {
		if _, err := a.Fetch(ctx); err != nil {
			return controller.RecordInfo{}, err
		}
	}
	r, err := a.c.SetIP(ctx, a.cur, ip)
	if err != nil {
		return controller.RecordInfo{}, err
	}
	a.cur = r
	return info(r), nil
}

func info(r arvan.Record) controller.RecordInfo {
	return controller.RecordInfo{IPs: r.IPs, Cloud: r.Cloud, TTL: r.TTL}
}

type logNotifier struct{}

func (logNotifier) Notify(html string) { log.Printf("notify: %s", html) }

// teeNotifier logs every notification and forwards it to Telegram.
type teeNotifier struct{ bot *telegram.Bot }

func (t teeNotifier) Notify(html string) {
	log.Printf("notify: %s", strings.ReplaceAll(html, "\n", " | "))
	t.bot.Notify(html)
}

func build(cfg *config.Config) (*arvanDNS, []probe.Prober, *telegram.Bot, error) {
	ac, err := arvan.New(cfg.Arvan.APIBase, cfg.Arvan.APIKey, cfg.Arvan.Domain, cfg.Arvan.Proxy)
	if err != nil {
		return nil, nil, nil, err
	}
	dns := &arvanDNS{c: ac, name: cfg.Arvan.Record, id: cfg.Arvan.RecordID}
	var probers []probe.Prober
	for _, s := range cfg.Servers {
		host := s.Probe.Host
		if host == "" {
			host = s.IP
		}
		p, err := probe.New(s.Probe.Type, host, s.Probe.Port)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("server %s: %w", s.Name, err)
		}
		probers = append(probers, p)
	}
	var bot *telegram.Bot
	if cfg.Telegram.BotToken != "" {
		bot, err = telegram.New(cfg.Telegram.APIBase, cfg.Telegram.BotToken, cfg.Telegram.ChatIDs, cfg.Telegram.Label, cfg.Telegram.Proxy)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return dns, probers, bot, nil
}

func needsEcho(cfg *config.Config) bool {
	for _, s := range cfg.Servers {
		if s.Probe.Type == "echo" {
			return true
		}
	}
	return false
}

func run(ctx context.Context, cfg *config.Config) error {
	dns, probers, bot, err := build(cfg)
	if err != nil {
		return err
	}
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		log.Printf("state file unreadable, starting fresh: %v", err)
	}
	if needsEcho(cfg) {
		go func() {
			if err := probe.ServeEcho(ctx, cfg.EchoListen); err != nil {
				log.Fatalf("echo server on %s: %v", cfg.EchoListen, err)
			}
		}()
		log.Printf("echo server listening on %s", cfg.EchoListen)
	}

	var notifier controller.Notifier = logNotifier{}
	if bot != nil {
		notifier = teeNotifier{bot}
		go bot.RunSender(ctx)
	}
	ctl := controller.New(controller.Options{
		Config:   cfg,
		DNS:      dns,
		Notifier: notifier,
		Lookup:   dnscheck.Lookup,
		Probers:  probers,
		State:    st,
		Save:     func(s state.State) error { return state.Save(cfg.StateFile, s) },
	})
	if bot != nil {
		go bot.RunCommands(ctx, func(cmd telegram.Command) {
			var reply string
			ctl.Do(ctx, func() { reply = ctl.Handle(ctx, cmd) })
			if reply != "" {
				if err := bot.Reply(ctx, cmd.ChatID, reply); err != nil {
					log.Printf("telegram reply: %v", err)
				}
			}
		})
	}
	log.Printf("backpack-plus %s: watching %d servers for %s", version, len(cfg.Servers), cfg.FQDN())
	ctl.Run(ctx)
	// Give the last notifications a moment to leave.
	time.Sleep(time.Second)
	return nil
}

func check(ctx context.Context, cfg *config.Config) error {
	dns, probers, bot, err := build(cfg)
	if err != nil {
		return err
	}
	ok := true
	fmt.Println("config: OK")

	if needsEcho(cfg) {
		ectx, cancel := context.WithCancel(ctx)
		defer cancel()
		errc := make(chan error, 1)
		go func() { errc <- probe.ServeEcho(ectx, cfg.EchoListen) }()
		select {
		case err := <-errc:
			// Already bound: most likely the backpack-plus service is running and answering.
			fmt.Printf("echo server: %s busy (%v) — assuming the running service answers\n", cfg.EchoListen, err)
		case <-time.After(200 * time.Millisecond):
			fmt.Printf("echo server: listening on %s for this check\n", cfg.EchoListen)
		}
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rec, err := dns.Fetch(rctx)
	cancel()
	if err != nil {
		ok = false
		fmt.Printf("arvan: FAILED: %v\n", err)
	} else {
		who := "unknown IP"
		if len(rec.IPs) == 1 {
			if i, found := cfg.ServerByIP(rec.IPs[0]); found {
				who = cfg.Servers[i].Name
			}
		}
		fmt.Printf("arvan: %s -> %v (%s), ttl %d, cdn %v, id %s\n", cfg.FQDN(), rec.IPs, who, rec.TTL, rec.Cloud, dns.id)
	}

	for i, p := range probers {
		pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout.D())
		r := p.Probe(pctx)
		cancel()
		s := cfg.Servers[i]
		if r.OK {
			fmt.Printf("probe %s (%s %s:%d): OK in %s\n", s.Name, s.Probe.Type, s.IP, s.Probe.Port, r.RTT.Round(time.Millisecond))
		} else {
			ok = false
			fmt.Printf("probe %s (%s %s:%d): FAILED: %v\n", s.Name, s.Probe.Type, s.IP, s.Probe.Port, r.Err)
		}
	}

	for _, res := range cfg.DNSVerify.Resolvers {
		lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		ips, err := dnscheck.Lookup(lctx, res, cfg.FQDN())
		cancel()
		if err != nil {
			fmt.Printf("resolver %s: %v\n", res, err)
		} else {
			fmt.Printf("resolver %s: %s -> %v\n", res, cfg.FQDN(), ips)
		}
	}

	if bot != nil {
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		for _, id := range cfg.Telegram.ChatIDs {
			if err := bot.Reply(tctx, id, "🧪 BackPack+ check: پیام آزمایشی — ربات درست کار می‌کند."); err != nil {
				ok = false
				fmt.Printf("telegram %d: FAILED: %v\n", id, err)
			} else {
				fmt.Printf("telegram %d: test message sent\n", id)
			}
		}
		cancel()
	} else {
		fmt.Println("telegram: not configured (notifications go to the log only)")
	}
	if !ok {
		return fmt.Errorf("check found problems")
	}
	fmt.Println("all checks passed")
	return nil
}
