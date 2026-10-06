package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mhbehzadian/backpack-plus/internal/config"
	"github.com/mhbehzadian/backpack-plus/internal/health"
	"github.com/mhbehzadian/backpack-plus/internal/probe"
	"github.com/mhbehzadian/backpack-plus/internal/state"
	"github.com/mhbehzadian/backpack-plus/internal/telegram"
)

const cfgJSON = `{
  "check_interval": "10s",
  "servers": [
    {"name": "iran1", "ip": "10.0.0.1", "tunnels": [{"name": "main", "port": 59999}, {"name": "panel", "port": 59997}]},
    {"name": "iran2", "ip": "10.0.0.2", "probe": {"port": 59999}}
  ],%s
  "failover": {"fail_threshold": 3, "recover_threshold": 3, "flap_window": "5m", "flap_max_drops": 3, "failback_after": "10m", "min_hold": "3m"},
  "arvan": {"api_key": "k", "domain": "example.com", "record": "tun"},
  "dns_verify": {"resolvers": ["r1", "r2"], "interval": "10s"}
}`

type fakeDNS struct {
	ip    string
	fail  bool
	sets  []string
	cloud bool
}

func (f *fakeDNS) Fetch(context.Context) (RecordInfo, error) {
	return RecordInfo{IPs: []string{f.ip}, TTL: 120, Cloud: f.cloud}, nil
}

func (f *fakeDNS) Set(_ context.Context, ip string) (RecordInfo, error) {
	if f.fail {
		return RecordInfo{}, errors.New("api down")
	}
	f.ip = ip
	f.sets = append(f.sets, ip)
	return RecordInfo{IPs: []string{ip}, TTL: 120, Cloud: f.cloud}, nil
}

type msgs []string

func (m *msgs) Notify(s string) { *m = append(*m, s) }

func (m *msgs) has(sub string) bool {
	for _, s := range *m {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

type harness struct {
	t     *testing.T
	c     *Controller
	dns   *fakeDNS
	out   *msgs
	now   time.Time
	resIP map[string]string // resolver -> answer
	saved state.State
}

func newHarness(t *testing.T, recordIP string) *harness {
	return newHarnessWith(t, recordIP, "", state.State{})
}

func newHarnessWith(t *testing.T, recordIP, extra string, st state.State) *harness {
	cfg, err := config.Parse([]byte(fmt.Sprintf(cfgJSON, extra)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dns: &fakeDNS{ip: recordIP}, out: &msgs{}, now: time.Unix(1_700_000_000, 0),
		resIP: map[string]string{"r1:53": recordIP, "r2:53": recordIP}}
	h.c = New(Options{
		Config:   cfg,
		DNS:      h.dns,
		Notifier: h.out,
		Lookup: func(_ context.Context, r, _ string) ([]string, error) {
			return []string{h.resIP[r]}, nil
		},
		Probes: []probe.Group{{}, {}},
		State:  st,
		Now:    func() time.Time { return h.now },
		Save:   func(s state.State) error { h.saved = s; return nil },
	})
	h.c.refreshRecord(context.Background(), true)
	return h
}

// step advances 10s and feeds one probe round: '+' ok, '-' failed, per server.
func (h *harness) step(pattern string) {
	h.now = h.now.Add(10 * time.Second)
	res := make([]probe.Result, len(pattern))
	for i, ch := range pattern {
		if ch == '+' {
			res[i] = probe.Result{OK: true, RTT: 50 * time.Millisecond}
		} else {
			res[i] = probe.Result{Err: errors.New("timeout")}
		}
	}
	h.c.Observe(res)
	h.c.Decide(context.Background())
	h.c.checkVerify(context.Background())
}

func (h *harness) steps(n int, pattern string) {
	for i := 0; i < n; i++ {
		h.step(pattern)
	}
}

func (h *harness) active() string {
	if h.c.active < 0 {
		return ""
	}
	return h.c.cfg.Servers[h.c.active].Name
}

func TestFailoverWhenPrimaryDies(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	if h.active() != "iran1" {
		t.Fatalf("active %q", h.active())
	}
	h.steps(2, "-+")
	if len(h.dns.sets) != 0 {
		t.Fatal("must not switch before fail_threshold")
	}
	h.step("-+")
	if h.active() != "iran2" || h.dns.ip != "10.0.0.2" {
		t.Fatalf("expected switch to iran2, got %q / %s", h.active(), h.dns.ip)
	}
	if !h.out.has("قطع است") || !h.out.has("رکورد دامین عوض شد") {
		t.Fatalf("missing notifications: %q", *h.out)
	}
	if h.saved.Active != "iran2" {
		t.Fatalf("state not saved: %+v", h.saved)
	}

	// DNS propagation: one resolver flips, then the other.
	h.resIP["r1:53"] = "10.0.0.2"
	h.step("-+")
	if h.out.has("حالا پشت دامین است") {
		t.Fatal("reported propagation before every resolver flipped")
	}
	h.resIP["r2:53"] = "10.0.0.2"
	h.steps(2, "-+")
	if !h.out.has("<b>iran2</b> حالا پشت دامین است") {
		t.Fatalf("missing propagation message: %q", *h.out)
	}
	if h.c.verify != nil {
		t.Fatal("verify job should be done")
	}
}

func TestFailbackAfterTenCleanMinutes(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	h.steps(3, "-+") // fail over
	if h.active() != "iran2" {
		t.Fatal("no failover")
	}
	h.steps(3, "++") // iran1 back up
	if !h.out.has("دوباره وصل شد") {
		t.Fatalf("missing recovery message: %q", *h.out)
	}
	h.steps(9*6, "++") // 9 more minutes
	if h.active() != "iran2" {
		t.Fatal("failed back too early")
	}
	h.steps(2*6, "++")
	if h.active() != "iran1" {
		t.Fatalf("expected failback to iran1, got %q", h.active())
	}
	if len(h.dns.sets) != 2 {
		t.Fatalf("sets %v", h.dns.sets)
	}
}

func TestFailbackClockResetsOnBlip(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	h.steps(3, "-+")
	h.steps(8*6, "++")
	h.step("-+") // a single drop on iran1 restarts its 10 minutes
	h.steps(5*6, "++")
	if h.active() != "iran2" {
		t.Fatal("failback must wait for a full clean period after a blip")
	}
	h.steps(6*6, "++")
	if h.active() != "iran1" {
		t.Fatal("expected failback eventually")
	}
}

func TestFlappingPrimarySwitches(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	for i := 0; i < 3; i++ { // never 3 failures in a row, but 3 drops in a minute
		h.step("-+")
		h.step("++")
	}
	if h.c.trackers[0].Status() != health.Unstable {
		t.Fatalf("iran1 status %s", h.c.trackers[0].Status())
	}
	if h.active() != "iran2" {
		t.Fatal("expected switch away from a flapping server")
	}
	if !h.out.has("ناپایدار") {
		t.Fatal("missing unstable message")
	}
}

func TestNoSwitchWhenBothDown(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	h.steps(5, "--")
	if len(h.dns.sets) != 0 {
		t.Fatal("must not move the record to a dead server")
	}
	if !h.out.has("هیچ سرور سالم دیگری") {
		t.Fatal("missing all-down alert")
	}
	n := len(*h.out)
	h.steps(5, "--")
	if len(*h.out) != n {
		t.Fatal("all-down alert must be rate limited")
	}
	h.steps(3, "-+") // iran2 comes back
	if h.active() != "iran2" {
		t.Fatal("expected switch once iran2 is healthy")
	}
}

func TestDeadPrimaryFallsBackToFlappingSecondary(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	for i := 0; i < 3; i++ { // iran2 flaps
		h.step("+-")
		h.step("++")
	}
	if h.c.trackers[1].Status() != health.Unstable {
		t.Fatalf("iran2 status %s", h.c.trackers[1].Status())
	}
	h.steps(3, "-+")
	if h.active() != "iran2" {
		t.Fatal("a dead primary must give way to an answering, unstable secondary")
	}
}

func TestDNSErrorRetries(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	h.dns.fail = true
	h.steps(5, "-+")
	if h.active() != "iran1" || !h.out.has("ناموفق بود") {
		t.Fatal("expected failure report and no state change")
	}
	h.dns.fail = false
	h.step("-+")
	if h.active() != "iran2" {
		t.Fatal("expected retry to succeed")
	}
}

func TestAutoOffOnlyAlerts(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.c.auto = false
	h.steps(3, "++")
	h.steps(4, "-+")
	if len(h.dns.sets) != 0 || !h.out.has("/switch iran2") {
		t.Fatalf("auto off must only suggest: %q", *h.out)
	}
}

func TestManualSwitchPinsUntilRelease(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	reply := h.c.Handle(context.Background(), telegram.Command{Name: "switch", Args: []string{"iran2"}})
	if h.active() != "iran2" || !strings.Contains(reply, "/release") {
		t.Fatalf("manual switch: %q", reply)
	}
	h.steps(15*6, "++")
	if h.active() != "iran2" {
		t.Fatal("pinned server must not fail back")
	}
	h.c.Handle(context.Background(), telegram.Command{Name: "release"})
	h.steps(19, "++") // min_hold already passed; iran1 clean for >10m
	if h.active() != "iran1" {
		t.Fatal("expected failback after release")
	}
	// Pinned server dying still fails over.
	h.c.Handle(context.Background(), telegram.Command{Name: "switch", Args: []string{"iran2"}})
	h.steps(3, "+-")
	if h.active() != "iran1" {
		t.Fatal("pinned server down must still fail over")
	}
}

func TestStartsOnSecondaryAndFailsBack(t *testing.T) {
	h := newHarness(t, "10.0.0.2") // service restarted while failed over
	if h.active() != "iran2" {
		t.Fatal("active must come from the record")
	}
	h.steps(10*6+1, "++")
	if h.active() != "iran1" {
		t.Fatal("expected failback after 10 clean minutes")
	}
}

func TestUnknownRecordIsCorrected(t *testing.T) {
	h := newHarness(t, "9.9.9.9")
	h.steps(3, "-+")
	if h.active() != "iran2" {
		t.Fatalf("expected record moved to the only healthy server, got %q", h.active())
	}
}

func TestManualChangeDetected(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	h.dns.ip = "10.0.0.2"
	h.c.refreshRecord(context.Background(), false)
	if h.active() != "iran2" || !h.out.has("دستی") {
		t.Fatal("manual change not noticed")
	}
}

func TestCloudRecordSkipsVerify(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.dns.cloud = true
	h.steps(3, "++")
	h.steps(3, "-+")
	if h.c.verify != nil || !h.out.has("CDN") {
		t.Fatal("CDN records take effect immediately")
	}
}

func TestStatusText(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "+-")
	s := h.c.StatusText()
	for _, want := range []string{"iran1", "iran2", "👉", "سالم", "قطع"} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q:\n%s", want, s)
		}
	}
}

func TestDurFA(t *testing.T) {
	if got := durFA(10 * time.Minute); got != "10 دقیقه" {
		t.Fatal(got)
	}
	if got := durFA(2*time.Minute + 5*time.Second); got != "2 دقیقه و 5 ثانیه" {
		t.Fatal(got)
	}
}

func TestPrimaryFromConfig(t *testing.T) {
	h := newHarnessWith(t, "10.0.0.1", `"primary": "iran2",`, state.State{})
	if h.c.cfg.Servers[h.c.primary].Name != "iran2" {
		t.Fatal("primary not taken from config")
	}
	// The record is on iran1; iran2 is the primary, so once iran2 has been
	// clean for failback_after the record moves to it.
	h.steps(10*6+1, "++")
	if h.active() != "iran2" {
		t.Fatalf("expected record on the primary, got %q", h.active())
	}
	// And iran1 is now the backup: iran2 failing moves the record to it.
	h.steps(3, "+-")
	if h.active() != "iran1" {
		t.Fatal("expected failover to iran1")
	}
	if !h.out.has("سرور مبنا") {
		t.Fatalf("messages should name the primary: %q", *h.out)
	}
}

func TestPrimaryCommand(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.steps(3, "++")
	reply := h.c.Handle(context.Background(), telegram.Command{Name: "primary", Args: []string{"iran2"}})
	if h.active() != "iran2" || !strings.Contains(reply, "منتقل شد") {
		t.Fatalf("primary switch: %q", reply)
	}
	if h.saved.Primary != "iran2" || h.saved.ConfigPrimary != "iran1" {
		t.Fatalf("primary not saved: %+v", h.saved)
	}
	// No failback to iran1 any more.
	h.steps(15*6, "++")
	if h.active() != "iran2" {
		t.Fatal("must stay on the new primary")
	}
	// Unhealthy server: becomes primary, record waits.
	h.steps(3, "-+")
	reply = h.c.Handle(context.Background(), telegram.Command{Name: "primary", Args: []string{"iran1"}})
	if h.active() != "iran2" || !strings.Contains(reply, "قطع") {
		t.Fatalf("unhealthy primary must not take the record yet: %q", reply)
	}
}

func TestPrimaryOverrideSurvivesRestartUntilConfigChanges(t *testing.T) {
	st := state.State{Primary: "iran2", ConfigPrimary: "iran1"}
	h := newHarnessWith(t, "10.0.0.2", "", st)
	if h.c.cfg.Servers[h.c.primary].Name != "iran2" {
		t.Fatal("bot choice must survive a restart")
	}
	h = newHarnessWith(t, "10.0.0.2", `"primary": "iran2",`, state.State{Primary: "iran1", ConfigPrimary: "iran1"})
	if h.c.cfg.Servers[h.c.primary].Name != "iran2" {
		t.Fatal("an edited config primary must win over an older bot choice")
	}
}

func TestStatusShowsTunnels(t *testing.T) {
	h := newHarness(t, "10.0.0.1")
	h.c.tunnels[0] = []probe.Result{{OK: true, RTT: 30 * time.Millisecond}, {Err: errors.New("x")}}
	h.step("-+")
	s := h.c.StatusText()
	for _, want := range []string{"🟢 main (59999)", "🔴 panel (59997)", "⭐️ مبنا"} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q:\n%s", want, s)
		}
	}
}
