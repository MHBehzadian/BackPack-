package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndFQDN(t *testing.T) {
	c, err := Parse([]byte(`{
	  "servers": [{"name":"a","ip":"1.1.1.1","probe":{"port":1}}, {"name":"b","ip":"2.2.2.2","probe":{"type":"tcp","port":443}}],
	  "failover": {"failback_after": 600},
	  "arvan": {"api_key":"k","domain":"example.com","record":"tun"},
	  "dns_verify": {"resolvers": ["8.8.8.8"]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Servers[0].Tunnels[0].Type != "echo" || c.CheckInterval.D() != 10*time.Second || !*c.Failover.Auto || !*c.Failover.RequireAllTunnels {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Failover.FailbackAfter.D() != 10*time.Minute {
		t.Fatalf("numeric seconds: %s", c.Failover.FailbackAfter.D())
	}
	if c.DNSVerify.Resolvers[0] != "8.8.8.8:53" {
		t.Fatalf("resolver port: %v", c.DNSVerify.Resolvers)
	}
	if c.FQDN() != "tun.example.com" {
		t.Fatal(c.FQDN())
	}
	if c.Primary != "a" {
		t.Fatalf("primary defaults to the first server, got %q", c.Primary)
	}
	if len(c.Servers[1].Tunnels) != 1 || c.Servers[1].Tunnels[0].Port != 443 || c.Servers[1].Tunnels[0].Type != "tcp" || c.Servers[1].Probe != nil {
		t.Fatalf("legacy probe not converted: %+v", c.Servers[1])
	}
}

func TestTunnelsAndPrimary(t *testing.T) {
	base := `{
	  "primary": %q,
	  "servers": [{"name":"a","ip":"1.1.1.1","tunnels":[{"name":"x","port":59999},{"port":59997}]},
	              {"name":"b","ip":"2.2.2.2","tunnels":[%s]}],
	  "arvan": {"api_key":"k","domain":"example.com"}
	}`
	c, err := Parse([]byte(fmt.Sprintf(base, "b", `{"port":59999}`)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Primary != "b" || c.Servers[0].Tunnels[1].Name != "port 59997" {
		t.Fatalf("%+v", c)
	}
	_, err = Parse([]byte(fmt.Sprintf(base, "zzz", `{"port":59999},{"port":59999}`)))
	if err == nil || !strings.Contains(err.Error(), "listed twice") || !strings.Contains(err.Error(), `primary "zzz"`) {
		t.Fatalf("expected duplicate-port and bad-primary errors, got %v", err)
	}
	_, err = Parse([]byte(fmt.Sprintf(base, "a", ``)))
	if err == nil || !strings.Contains(err.Error(), "at least one tunnel") {
		t.Fatalf("expected missing-tunnel error, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	_, err := Parse([]byte(`{
	  "servers": [{"name":"a","ip":"nope","probe":{"port":1}}],
	  "arvan": {"domain":"example.com"},
	  "telegram": {"bot_token":"x"}
	}`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"two servers", "not an IP", "api_key", "chat_ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if _, err := Parse([]byte(`{"typo": 1}`)); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Errorf("unknown fields must be rejected: %v", err)
	}
}

func TestExampleConfigParses(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 2 || c.FQDN() != "tun.example.com" {
		t.Fatalf("unexpected example: %+v", c)
	}
}
