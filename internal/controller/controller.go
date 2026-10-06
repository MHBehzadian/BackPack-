// Package controller decides which Iran server the domain points at.
//
// Every server's tunnel is probed continuously. When the server the record
// points at goes down or keeps dropping, the record is moved to the next
// healthy server in priority order; when a higher-priority server has been
// clean for FailbackAfter, the record moves back. Every step is reported.
//
// The controller is single-threaded: Run owns all state, and bot commands are
// handed to it over a channel.
package controller

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mhbehzadian/backpack-plus/internal/config"
	"github.com/mhbehzadian/backpack-plus/internal/health"
	"github.com/mhbehzadian/backpack-plus/internal/probe"
	"github.com/mhbehzadian/backpack-plus/internal/state"
	"github.com/mhbehzadian/backpack-plus/internal/telegram"
)

// RecordInfo is what the controller needs to know about the DNS record.
type RecordInfo struct {
	IPs   []string
	Cloud bool // proxied through the Arvan CDN: the switch takes effect at the edge
	TTL   int
}

// DNS reads and re-points the managed record.
type DNS interface {
	Fetch(ctx context.Context) (RecordInfo, error)
	Set(ctx context.Context, ip string) (RecordInfo, error)
}

// Notifier delivers messages (Telegram, or the log when no bot is configured).
type Notifier interface {
	Notify(html string)
}

// LookupFunc asks one resolver for the IPv4 addresses of name.
type LookupFunc func(ctx context.Context, resolver, name string) ([]string, error)

// recordRefresh is how often the record is re-read to notice manual changes.
const recordRefresh = 5 * time.Minute

// errorRepeat is how often a persisting error (DNS API down, no healthy server) is re-announced.
const errorRepeat = 15 * time.Minute

type Controller struct {
	cfg     *config.Config
	dns     DNS
	notify  Notifier
	lookup  LookupFunc
	probers []probe.Prober
	now     func() time.Time
	save    func(state.State) error

	trackers []*health.Tracker

	recordKnown   bool
	record        RecordInfo
	lastRecordAt  time.Time
	active        int // index into cfg.Servers, -1 when the record points elsewhere
	lastSwitch    time.Time
	auto          bool
	pinned        bool
	verify        *verifyJob
	lastAnnounced map[string]time.Time // rate limit for repeating warnings

	cmds chan func()
}

type verifyJob struct {
	target    int
	ip        string
	oldIP     string
	started   time.Time
	lastCheck time.Time
	seen      map[string]time.Time // resolver -> when it first answered the new IP
	timedOut  bool
}

type Options struct {
	Config   *config.Config
	DNS      DNS
	Notifier Notifier
	Lookup   LookupFunc
	Probers  []probe.Prober // one per server, same order as Config.Servers
	State    state.State
	Save     func(state.State) error
	Now      func() time.Time
}

func New(o Options) *Controller {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Save == nil {
		o.Save = func(state.State) error { return nil }
	}
	c := &Controller{
		cfg:           o.Config,
		dns:           o.DNS,
		notify:        o.Notifier,
		lookup:        o.Lookup,
		probers:       o.Probers,
		now:           o.Now,
		save:          o.Save,
		active:        -1,
		auto:          *o.Config.Failover.Auto,
		pinned:        o.State.Pinned,
		lastSwitch:    o.State.LastSwitch,
		lastAnnounced: map[string]time.Time{},
		cmds:          make(chan func(), 16),
	}
	if o.State.Auto != nil {
		c.auto = *o.State.Auto
	}
	if i, ok := c.cfg.ServerByName(o.State.Active); ok {
		c.active = i
	}
	p := health.Params{
		FailThreshold:    c.cfg.Failover.FailThreshold,
		RecoverThreshold: c.cfg.Failover.RecoverThreshold,
		FlapWindow:       c.cfg.Failover.FlapWindow.D(),
		FlapMaxDrops:     c.cfg.Failover.FlapMaxDrops,
	}
	now := c.now()
	for range c.cfg.Servers {
		c.trackers = append(c.trackers, health.NewTracker(p, now))
	}
	return c
}

// Run probes every CheckInterval and acts on the results until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	c.refreshRecord(ctx, true)
	c.announceStart()
	t := time.NewTicker(c.cfg.CheckInterval.D())
	defer t.Stop()
	c.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-c.cmds:
			f()
		case <-t.C:
			c.Tick(ctx)
		}
	}
}

// Tick runs one probe round and everything that follows from it.
func (c *Controller) Tick(ctx context.Context) {
	c.Observe(c.probeAll(ctx))
	if !c.recordKnown || c.now().Sub(c.lastRecordAt) >= recordRefresh {
		c.refreshRecord(ctx, false)
	}
	c.Decide(ctx)
	c.checkVerify(ctx)
}

func (c *Controller) probeAll(ctx context.Context) []probe.Result {
	res := make([]probe.Result, len(c.probers))
	var wg sync.WaitGroup
	for i, p := range c.probers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, c.cfg.ProbeTimeout.D())
			defer cancel()
			res[i] = p.Probe(pctx)
		}()
	}
	wg.Wait()
	return res
}

// Observe feeds one round of probe results into the trackers and reports
// status changes.
func (c *Controller) Observe(results []probe.Result) {
	now := c.now()
	for i, r := range results {
		tr := c.trackers[i]
		old, cur := tr.Record(now, r.OK, r.RTT, r.Err)
		if !r.OK {
			log.Printf("probe %s: %v", c.cfg.Servers[i].Name, r.Err)
		}
		if old != cur {
			log.Printf("server %s: %s -> %s", c.cfg.Servers[i].Name, old, cur)
			c.announceStatus(i, old, cur)
		}
	}
}

// Decide moves the record when the active server is unusable, or back to a
// higher-priority server once it has been clean long enough.
func (c *Controller) Decide(ctx context.Context) {
	if !c.recordKnown {
		return
	}
	now := c.now()
	f := c.cfg.Failover

	if c.active < 0 {
		target := c.bestUsable(-1)
		if target < 0 {
			return
		}
		ip := strings.Join(c.record.IPs, ", ")
		if !c.auto {
			c.announceOnce("unknown-ip", fmt.Sprintf("⚠️ رکورد <code>%s</code> به آی‌پی <code>%s</code> اشاره می‌کند که جزو سرورهای تعریف‌شده نیست.\nحالت خودکار خاموش است؛ برای انتقال: <code>/switch %s</code>",
				esc(c.cfg.FQDN()), esc(ip), esc(c.cfg.Servers[target].Name)))
			return
		}
		c.switchTo(ctx, target, fmt.Sprintf("رکورد به آی‌پی ناشناس <code>%s</code> اشاره می‌کرد", esc(ip)))
		return
	}

	st := c.trackers[c.active].Status()
	if st == health.Down || st == health.Unstable {
		target := c.bestUsable(c.active)
		if target < 0 && st == health.Down {
			// A flapping server that is answering right now beats a dead one.
			target = c.bestAnswering(c.active)
		}
		if target < 0 {
			c.announceOnce("no-target", fmt.Sprintf("🚨 سرور فعال <b>%s</b> %s است و هیچ سرور سالم دیگری برای جابه‌جایی وجود ندارد. رکورد دست نخورد؛ مدام چک می‌کنم.",
				esc(c.cfg.Servers[c.active].Name), statusFA(st)))
			return
		}
		if st == health.Unstable && now.Sub(c.lastSwitch) < f.MinHold.D() {
			return // just switched; let things settle before moving on a flap
		}
		why := fmt.Sprintf("سرور <b>%s</b> %s شد", esc(c.cfg.Servers[c.active].Name), statusFA(st))
		if st == health.Unstable {
			why = fmt.Sprintf("سرور <b>%s</b> ناپایدار است (%d قطعی در %s اخیر)", esc(c.cfg.Servers[c.active].Name),
				c.trackers[c.active].DropsInWindow(), durFA(f.FlapWindow.D()))
		}
		if !c.auto {
			c.announceOnce("manual-needed", fmt.Sprintf("⚠️ %s ولی حالت خودکار خاموش است.\nبرای انتقال به سرور سالم: <code>/switch %s</code>", why, esc(c.cfg.Servers[target].Name)))
			return
		}
		c.switchTo(ctx, target, why)
		return
	}

	// Failback: a higher-priority server that has been clean long enough.
	if !c.auto || c.pinned || now.Sub(c.lastSwitch) < f.MinHold.D() {
		return
	}
	for i := 0; i < c.active; i++ {
		tr := c.trackers[i]
		if tr.Status() == health.Up && tr.CleanFor(now) >= f.FailbackAfter.D() {
			c.switchTo(ctx, i, fmt.Sprintf("سرور <b>%s</b> (اولویت بالاتر) %s بدون قطعی پایدار بوده؛ برگشت به آن",
				esc(c.cfg.Servers[i].Name), durFA(tr.CleanFor(now).Round(time.Second))))
			return
		}
	}
}

// bestUsable is the highest-priority Up server other than skip, or -1.
func (c *Controller) bestUsable(skip int) int {
	for i, tr := range c.trackers {
		if i != skip && tr.Status().Usable() {
			return i
		}
	}
	return -1
}

// bestAnswering is the highest-priority Unstable server whose last probe
// succeeded, or -1.
func (c *Controller) bestAnswering(skip int) int {
	for i, tr := range c.trackers {
		if i != skip && tr.Status() == health.Unstable && tr.LastErr == nil {
			return i
		}
	}
	return -1
}

func (c *Controller) switchTo(ctx context.Context, target int, why string) {
	srv := c.cfg.Servers[target]
	from := "—"
	oldIP := strings.Join(c.record.IPs, ",")
	if c.active >= 0 {
		from = c.cfg.Servers[c.active].Name
	}
	rec, err := c.dns.Set(ctx, srv.IP)
	if err == nil && !slices.Contains(rec.IPs, srv.IP) {
		err = fmt.Errorf("Arvan accepted the update but the record now holds %v", rec.IPs)
	}
	if err != nil {
		log.Printf("switch to %s failed: %v", srv.Name, err)
		c.announceOnce("dns-error", fmt.Sprintf("❌ %s\nتلاش برای تغییر رکورد به <b>%s</b> ناموفق بود:\n<code>%s</code>\nدوباره تلاش می‌کنم.",
			why, esc(srv.Name), esc(err.Error())))
		return
	}
	delete(c.lastAnnounced, "dns-error")
	delete(c.lastAnnounced, "no-target")
	delete(c.lastAnnounced, "manual-needed")
	delete(c.lastAnnounced, "unknown-ip")
	now := c.now()
	c.record = rec
	c.lastRecordAt = now
	c.active = target
	c.lastSwitch = now
	if target == 0 {
		c.pinned = false
	}
	c.persist()
	log.Printf("record %s switched %s -> %s (%s)", c.cfg.FQDN(), from, srv.Name, srv.IP)

	msg := fmt.Sprintf("🔁 <b>رکورد دامین عوض شد</b>\n%s\n<code>%s</code> از <b>%s</b> به <b>%s</b> (<code>%s</code>)",
		why, esc(c.cfg.FQDN()), esc(from), esc(srv.Name), esc(srv.IP))
	if rec.Cloud {
		msg += "\n☁️ رکورد پشت CDN آروان است؛ تغییر همین حالا روی لبهٔ آروان اعمال شد."
		c.verify = nil
	} else {
		msg += fmt.Sprintf("\n⏳ منتظر می‌مانم تا DNSها آی‌پی جدید را برگردانند (TTL: %d ثانیه)…", rec.TTL)
		c.verify = &verifyJob{target: target, ip: srv.IP, oldIP: oldIP, started: now, seen: map[string]time.Time{}}
	}
	c.notify.Notify(msg)
}

// checkVerify watches public resolvers until they all return the new IP.
func (c *Controller) checkVerify(ctx context.Context) {
	v := c.verify
	if v == nil || c.lookup == nil {
		return
	}
	now := c.now()
	if now.Sub(v.lastCheck) < c.cfg.DNSVerify.Interval.D() {
		return
	}
	v.lastCheck = now
	name := c.cfg.FQDN()
	type ans struct {
		ips []string
		err error
	}
	answers := make([]ans, len(c.cfg.DNSVerify.Resolvers))
	var wg sync.WaitGroup
	for i, r := range c.cfg.DNSVerify.Resolvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			ips, err := c.lookup(lctx, r, name)
			answers[i] = ans{ips, err}
		}()
	}
	wg.Wait()

	var pending []string
	for i, r := range c.cfg.DNSVerify.Resolvers {
		a := answers[i]
		if a.err == nil && len(a.ips) > 0 && !slices.ContainsFunc(a.ips, func(ip string) bool { return ip != v.ip }) {
			if _, ok := v.seen[r]; !ok {
				v.seen[r] = now
			}
			continue
		}
		got := strings.Join(a.ips, ",")
		if a.err != nil {
			got = "error"
		}
		pending = append(pending, fmt.Sprintf("%s → %s", r, got))
	}
	srv := c.cfg.Servers[v.target]
	if len(pending) == 0 {
		tr := c.trackers[v.target]
		c.notify.Notify(fmt.Sprintf("✅ <b>%s</b> حالا پشت دامین است\nهمهٔ DNSهای بررسی‌شده آی‌پی <code>%s</code> را برمی‌گردانند (بعد از %s).\nترافیک کاربران از تانل <b>%s</b> عبور می‌کند — وضعیت تانل: %s",
			esc(srv.Name), esc(v.ip), durFA(now.Sub(v.started).Round(time.Second)), esc(srv.Name), statusFA(tr.Status())))
		c.verify = nil
		return
	}
	if !v.timedOut && now.Sub(v.started) >= c.cfg.DNSVerify.Timeout.D() {
		v.timedOut = true
		c.notify.Notify(fmt.Sprintf("⚠️ %s از تغییر رکورد گذشته ولی این DNSها هنوز آی‌پی جدید (<code>%s</code>) را نمی‌دهند:\n<code>%s</code>\nبه چک کردن ادامه می‌دهم.",
			durFA(now.Sub(v.started).Round(time.Second)), esc(v.ip), esc(strings.Join(pending, "\n"))))
	}
}

// refreshRecord re-reads the record from Arvan and notices manual changes.
func (c *Controller) refreshRecord(ctx context.Context, startup bool) {
	rec, err := c.dns.Fetch(ctx)
	if err != nil {
		log.Printf("read record: %v", err)
		c.announceOnce("dns-read", fmt.Sprintf("❌ خواندن رکورد <code>%s</code> از آروان ناموفق بود:\n<code>%s</code>", esc(c.cfg.FQDN()), esc(err.Error())))
		return
	}
	delete(c.lastAnnounced, "dns-read")
	c.record = rec
	c.recordKnown = true
	c.lastRecordAt = c.now()

	idx := -1
	if len(rec.IPs) == 1 {
		idx, _ = c.cfg.ServerByIP(rec.IPs[0])
	}
	if idx == c.active {
		return
	}
	prev := c.active
	c.active = idx
	c.persist()
	if startup {
		return // announced by announceStart
	}
	from := "—"
	if prev >= 0 {
		from = c.cfg.Servers[prev].Name
	}
	to := strings.Join(rec.IPs, ", ")
	if idx >= 0 {
		to = c.cfg.Servers[idx].Name
	}
	c.verify = nil
	c.notify.Notify(fmt.Sprintf("✋ رکورد <code>%s</code> بیرون از BackPack+ (دستی) عوض شده: از <b>%s</b> به <b>%s</b>. از این به بعد همین را مبنا می‌گیرم.",
		esc(c.cfg.FQDN()), esc(from), esc(to)))
}

func (c *Controller) persist() {
	s := state.State{LastSwitch: c.lastSwitch, Pinned: c.pinned}
	auto := c.auto
	s.Auto = &auto
	if c.active >= 0 {
		s.Active = c.cfg.Servers[c.active].Name
	}
	if err := c.save(s); err != nil {
		log.Printf("save state: %v", err)
	}
}

// announceOnce sends msg, then stays quiet about the same key for errorRepeat.
func (c *Controller) announceOnce(key, msg string) {
	now := c.now()
	if t, ok := c.lastAnnounced[key]; ok && now.Sub(t) < errorRepeat {
		return
	}
	c.lastAnnounced[key] = now
	c.notify.Notify(msg)
}

func (c *Controller) announceStart() {
	var b strings.Builder
	b.WriteString("🚀 <b>BackPack+ شروع به کار کرد</b>\n")
	fmt.Fprintf(&b, "دامین: <code>%s</code>\n", esc(c.cfg.FQDN()))
	if c.recordKnown {
		if c.active >= 0 {
			fmt.Fprintf(&b, "الان پشت دامین: <b>%s</b>\n", esc(c.cfg.Servers[c.active].Name))
		} else {
			fmt.Fprintf(&b, "الان پشت دامین: <code>%s</code> (ناشناس)\n", esc(strings.Join(c.record.IPs, ", ")))
		}
	}
	b.WriteString("سرورها به ترتیب اولویت: ")
	for i, s := range c.cfg.Servers {
		if i > 0 {
			b.WriteString(" ← ")
		}
		b.WriteString(esc(s.Name))
	}
	fmt.Fprintf(&b, "\nحالت خودکار: %s", onOff(c.auto))
	c.notify.Notify(b.String())
}

func (c *Controller) announceStatus(i int, old, cur health.Status) {
	s := c.cfg.Servers[i]
	tr := c.trackers[i]
	name := esc(s.Name)
	role := ""
	if i == c.active {
		role = " (فعال، پشت دامین)"
	} else {
		role = " (رزرو)"
	}
	var msg string
	switch cur {
	case health.Down:
		reason := ""
		if tr.LastErr != nil {
			reason = "\nعلت: <code>" + esc(tr.LastErr.Error()) + "</code>"
		}
		msg = fmt.Sprintf("🔴 تانل سرور <b>%s</b>%s قطع است.%s", name, role, reason)
	case health.Unstable:
		msg = fmt.Sprintf("🟠 تانل سرور <b>%s</b>%s ناپایدار است: %d قطعی در %s اخیر.",
			name, role, tr.DropsInWindow(), durFA(c.cfg.Failover.FlapWindow.D()))
	case health.Up:
		if old == health.Unknown {
			return // first good probes after start; covered by the startup message
		}
		msg = fmt.Sprintf("🟢 تانل سرور <b>%s</b>%s دوباره وصل شد (RTT %s).", name, role, tr.LastRTT.Round(time.Millisecond))
		if c.active > i {
			if c.auto && !c.pinned {
				msg += fmt.Sprintf("\nاگر %s بدون قطعی بماند، رکورد را به آن برمی‌گردانم.", durFA(c.cfg.Failover.FailbackAfter.D()))
			} else {
				msg += "\nبرگشت خودکار غیرفعال است (" + c.whyNoFailback() + ")."
			}
		}
	default:
		return
	}
	c.notify.Notify(msg)
}

func (c *Controller) whyNoFailback() string {
	if !c.auto {
		return "حالت خودکار خاموش؛ <code>/auto on</code>"
	}
	return "سوییچ دستی؛ <code>/release</code>"
}

// Do runs f on the controller goroutine and waits for it.
func (c *Controller) Do(ctx context.Context, f func()) {
	done := make(chan struct{})
	select {
	case c.cmds <- func() { f(); close(done) }:
	case <-ctx.Done():
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Handle executes a bot command and returns the reply. Call it through Do.
func (c *Controller) Handle(ctx context.Context, cmd telegram.Command) string {
	switch cmd.Name {
	case "start", "help":
		return helpText
	case "status":
		return c.StatusText()
	case "auto":
		if len(cmd.Args) == 0 {
			return "حالت خودکار: " + onOff(c.auto) + "\n<code>/auto on</code> یا <code>/auto off</code>"
		}
		switch strings.ToLower(cmd.Args[0]) {
		case "on":
			c.auto = true
		case "off":
			c.auto = false
		default:
			return "<code>/auto on</code> یا <code>/auto off</code>"
		}
		delete(c.lastAnnounced, "manual-needed")
		c.persist()
		return "حالت خودکار: " + onOff(c.auto)
	case "release":
		c.pinned = false
		c.persist()
		return "سوییچ دستی آزاد شد؛ برگشت خودکار به سرور اصلی دوباره فعال است."
	case "switch":
		if len(cmd.Args) == 0 {
			return "استفاده: <code>/switch نام‌سرور</code> (برای سرور ناسالم: <code>/switch نام force</code>)"
		}
		i, ok := c.cfg.ServerByName(cmd.Args[0])
		if !ok {
			return "سروری با این نام نیست: " + esc(cmd.Args[0])
		}
		force := len(cmd.Args) > 1 && strings.EqualFold(cmd.Args[1], "force")
		if st := c.trackers[i].Status(); !st.Usable() && !force {
			return fmt.Sprintf("سرور <b>%s</b> الان %s است. اگر مطمئنی: <code>/switch %s force</code>", esc(c.cfg.Servers[i].Name), statusFA(st), esc(c.cfg.Servers[i].Name))
		}
		if !c.recordKnown {
			c.refreshRecord(ctx, false)
			if !c.recordKnown {
				return "رکورد از آروان خوانده نشد؛ بعداً دوباره امتحان کن."
			}
		}
		if i == c.active {
			return "رکورد همین الان روی <b>" + esc(c.cfg.Servers[i].Name) + "</b> است."
		}
		c.pinned = i != 0
		c.switchTo(ctx, i, "سوییچ دستی از ربات")
		if c.active != i {
			return "تغییر رکورد ناموفق بود؛ جزئیات در پیام بالا."
		}
		if c.pinned {
			return "انجام شد. برگشت خودکار به سرور اصلی تا <code>/release</code> متوقف است (اگر این سرور قطع شود، باز هم خودکار جابه‌جا می‌کنم)."
		}
		return "انجام شد."
	}
	return "دستور ناشناخته. /help"
}

// StatusText is the /status report.
func (c *Controller) StatusText() string {
	now := c.now()
	var b strings.Builder
	fmt.Fprintf(&b, "📊 <b>وضعیت BackPack+</b>\nدامین: <code>%s</code>\n", esc(c.cfg.FQDN()))
	switch {
	case !c.recordKnown:
		b.WriteString("رکورد: هنوز از آروان خوانده نشده\n")
	case c.active < 0:
		fmt.Fprintf(&b, "رکورد: <code>%s</code> (ناشناس)\n", esc(strings.Join(c.record.IPs, ", ")))
	default:
		fmt.Fprintf(&b, "پشت دامین: <b>%s</b>", esc(c.cfg.Servers[c.active].Name))
		if !c.lastSwitch.IsZero() {
			fmt.Fprintf(&b, " (آخرین تغییر: %s پیش)", durFA(now.Sub(c.lastSwitch).Truncate(time.Minute)))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "حالت خودکار: %s", onOff(c.auto))
	if c.pinned {
		b.WriteString(" — سوییچ دستی (برگشت خودکار متوقف، <code>/release</code>)")
	}
	b.WriteString("\n\n")
	for i, s := range c.cfg.Servers {
		tr := c.trackers[i]
		mark := "▫️"
		if i == c.active {
			mark = "👉"
		}
		fmt.Fprintf(&b, "%s %s <b>%s</b> <code>%s</code>\n", mark, statusIcon(tr.Status()), esc(s.Name), esc(s.IP))
		fmt.Fprintf(&b, "    وضعیت: %s", statusFA(tr.Status()))
		if tr.Status() == health.Up {
			fmt.Fprintf(&b, " — RTT %s — پایدار از %s پیش", tr.LastRTT.Round(time.Millisecond), durFA(tr.CleanFor(now).Truncate(time.Second)))
		}
		fmt.Fprintf(&b, "\n    قطعی در %s اخیر: %d", durFA(c.cfg.Failover.FlapWindow.D()), tr.DropsInWindow())
		if tr.LastErr != nil {
			fmt.Fprintf(&b, "\n    آخرین خطا: <code>%s</code>", esc(tr.LastErr.Error()))
		}
		b.WriteString("\n")
	}
	if c.verify != nil {
		fmt.Fprintf(&b, "\n⏳ در انتظار انتشار DNS به <b>%s</b> (%d از %d DNS، %s گذشته)",
			esc(c.cfg.Servers[c.verify.target].Name), len(c.verify.seen), len(c.cfg.DNSVerify.Resolvers), durFA(now.Sub(c.verify.started).Round(time.Second)))
	}
	return b.String()
}

const helpText = `<b>BackPack+</b> — دستورها:
/status — وضعیت سرورها و دامین
/switch نام — انتقال دستی رکورد به یک سرور
/release — آزاد کردن سوییچ دستی (برگشت خودکار به سرور اصلی)
/auto on|off — روشن/خاموش کردن جابه‌جایی خودکار`

func esc(s string) string { return telegram.Escape(s) }

func onOff(b bool) string {
	if b {
		return "روشن ✅"
	}
	return "خاموش ⛔️"
}

func statusFA(s health.Status) string {
	switch s {
	case health.Up:
		return "سالم"
	case health.Unstable:
		return "ناپایدار"
	case health.Down:
		return "قطع"
	}
	return "نامشخص"
}

func statusIcon(s health.Status) string {
	switch s {
	case health.Up:
		return "🟢"
	case health.Unstable:
		return "🟠"
	case health.Down:
		return "🔴"
	}
	return "⚪️"
}

// durFA formats a duration in short Persian ("۱۰ دقیقه", "۲ دقیقه و ۵ ثانیه").
func durFA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d ساعت", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d دقیقه", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d ثانیه", s))
	}
	return strings.Join(parts, " و ")
}
