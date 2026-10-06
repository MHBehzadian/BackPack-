package config

import (
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
	if c.Servers[0].Probe.Type != "echo" || c.CheckInterval.D() != 10*time.Second || !*c.Failover.Auto {
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
