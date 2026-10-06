package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhbehzadian/backpack-plus/internal/config"
)

func mockAPIs(t *testing.T, recordIP string) (*httptest.Server, *[]string) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(b, &m)
			if m["chat_id"].(float64) == 999 {
				io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
				return
			}
			sent = append(sent, m["text"].(string))
			io.WriteString(w, `{"ok":true,"result":{}}`)
		case strings.Contains(r.URL.Path, "/domains/example.com/dns-records"):
			if r.Header.Get("Authorization") != "Apikey good-key-123456" {
				w.WriteHeader(401)
				io.WriteString(w, `{"message":"Unauthenticated."}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"r1","type":"a","name":"tun","value":[{"ip":"`+recordIP+`"}],"ttl":120,"cloud":false},`+
				`{"id":"r2","type":"a","name":"@","value":[{"ip":"`+recordIP+`"}],"ttl":120,"cloud":false}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

func seed(t *testing.T, path, api string) {
	b := `{"arvan":{"api_base":"` + api + `/cdn/4.0"},"telegram":{"api_base":"` + api + `"}}`
	if err := os.WriteFile(path, []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupWizardFresh(t *testing.T) {
	api, sent := mockAPIs(t, "1.1.1.1")
	path := filepath.Join(t.TempDir(), "config.json")
	seed(t, path, api.URL)

	answers := strings.Join([]string{
		"",        // 2 servers
		"",        // name iran1
		"nope",    // bad ip -> asked again
		"1.1.1.1", // ip
		"59999:main,59997:panel,59995:x-ray",
		"bad name",    // name with space -> asked again
		"iran2",       // name
		"1.1.1.1",     // duplicate ip -> asked again
		"2.2.2.2",     //
		"59999",       // one tunnel
		"3",           // invalid primary -> asked again
		"iran2",       // primary by name
		"wrong-key",   // arvan key (fails)
		"example.com", //
		"tun",         //
		"",            // enter again? default yes
		"good-key-123456",
		"",                // keep domain
		"tun.example.com", // full name is accepted
		"123:ABC",         // bot token
		"999",             // chat id that fails
		"",                // no label
		"y",               // again
		"",                // keep token
		"42, 43",          //
		"Kharej-DE",       // label
		"",                // failback 10
		"",                // interval 10
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := setup(context.Background(), path, strings.NewReader(answers), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	o := out.String()
	for _, want := range []string{"enter an IPv4 address", "no spaces", "already used by another server",
		"pick one of the servers", "Unauthenticated", "tun.example.com → 1.1.1.1 (= iran1)", "chat not found", "OK — check Telegram"} {
		if !strings.Contains(o, want) {
			t.Errorf("output missing %q", want)
		}
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if c.Primary != "iran2" || len(c.Servers) != 2 || len(c.Servers[0].Tunnels) != 3 || c.Servers[0].Tunnels[2].Name != "x-ray" {
		t.Fatalf("wrong config: %+v", c)
	}
	if c.Arvan.APIKey != "good-key-123456" || c.Arvan.Record != "tun" || c.FQDN() != "tun.example.com" {
		t.Fatalf("arvan: %+v", c.Arvan)
	}
	if len(c.Telegram.ChatIDs) != 2 || c.Telegram.Label != "Kharej-DE" || len(*sent) != 2 {
		t.Fatalf("telegram: %+v sent=%v", c.Telegram, *sent)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`"min_hold": "3m0s"`, `"fail_threshold": 3`, `"echo_listen": "127.0.0.1:59998"`, `"auto": true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("written config should spell out %s:\n%s", want, raw)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config must be private, got %v", fi.Mode().Perm())
	}
}

func TestSetupWizardEditKeepsValues(t *testing.T) {
	api, _ := mockAPIs(t, "2.2.2.2")
	path := filepath.Join(t.TempDir(), "config.json")
	seed(t, path, api.URL)
	first := "\n\n1.1.1.1\n59999\n\n2.2.2.2\n59999\n\ngood-key-123456\nexample.com\n@\n\n\n\n"
	var firstOut bytes.Buffer
	if err := setup(context.Background(), path, strings.NewReader(first), &firstOut); err != nil {
		t.Fatalf("%v\n%s", err, firstOut.String())
	}
	if !strings.Contains(firstOut.String(), "example.com → 2.2.2.2 (= iran2)") {
		t.Fatalf("first run:\n%s", firstOut.String())
	}
	// Second run: Enter everywhere keeps every value, including the masked key.
	var out bytes.Buffer
	if err := setup(context.Background(), path, strings.NewReader(strings.Repeat("\n", 20)), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Arvan.APIKey != "good-key-123456" || c.FQDN() != "example.com" || c.Primary != "iran1" || c.Telegram.BotToken != "" {
		t.Fatalf("values not kept: %+v %+v", c.Arvan, c.Telegram)
	}
	if !strings.Contains(out.String(), "example.com → 2.2.2.2 (= iran2)") {
		t.Fatalf("record check on edit:\n%s", out.String())
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatal("previous config must be kept as .bak")
	}
}

func TestSetupAbortsOnEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := setup(context.Background(), path, strings.NewReader("3\n"), io.Discard)
	if err != errAborted {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("nothing may be written when setup is aborted")
	}
}

func TestParseTunnels(t *testing.T) {
	ts, err := parseTunnels(" 59999:main , 59997 ")
	if err != nil || len(ts) != 2 || ts[0].Name != "main" || ts[1].Port != 59997 {
		t.Fatalf("%v %v", ts, err)
	}
	for _, bad := range []string{"", "x", "70000", "1,1", "5:a b"} {
		if _, err := parseTunnels(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := formatTunnels([]config.Tunnel{{Name: "main", Port: 1}, {Name: "port 2", Port: 2}}); got != "1:main,2" {
		t.Fatal(got)
	}
}
