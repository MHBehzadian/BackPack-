package arvan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const recJSON = `{"id":"r1","type":"a","name":"tun","value":[{"ip":"1.1.1.1","port":null,"weight":100,"country":""}],"ttl":120,"cloud":false,"upstream_https":"default","ip_filter_mode":{"count":"single","order":"none","geo_filter":"none"},"is_protected":false,"created_at":"x"}`

func TestFindAndSetIP(t *testing.T) {
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Apikey secret" {
			t.Errorf("auth header %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/domains/example.com/dns-records":
			io.WriteString(w, `{"data":[{"id":"x","type":"cname","name":"tun","value":{"host":"a"}},`+recJSON+`]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/domains/example.com/dns-records/r1":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &put)
			out := strings.Replace(recJSON, "1.1.1.1", "2.2.2.2", 1)
			io.WriteString(w, `{"data":`+out+`,"message":"updated"}`)
		default:
			http.Error(w, `{"message":"nope"}`, 404)
		}
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "secret", "example.com", "")
	rec, err := c.FindA(context.Background(), "tun.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "r1" || len(rec.IPs) != 1 || rec.IPs[0] != "1.1.1.1" || rec.TTL != 120 {
		t.Fatalf("bad record %+v", rec)
	}
	got, err := c.SetIP(context.Background(), rec, "2.2.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if got.IPs[0] != "2.2.2.2" {
		t.Fatalf("after set: %v", got.IPs)
	}
	if put["ttl"].(float64) != 120 || put["cloud"].(bool) || put["name"] != "tun" {
		t.Fatalf("settings not preserved: %v", put)
	}
	if _, ok := put["is_protected"]; ok {
		t.Fatal("read-only fields must not be sent")
	}
	v := put["value"].([]any)[0].(map[string]any)
	if v["ip"] != "2.2.2.2" || v["weight"].(float64) != 100 {
		t.Fatalf("value %v", v)
	}
	if _, ok := v["port"]; ok {
		t.Fatal("null port must be dropped")
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"message":"Unauthenticated."}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "Apikey bad", "example.com", "")
	_, err := c.FindA(context.Background(), "@")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Unauthenticated") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormName(t *testing.T) {
	for _, tc := range [][3]string{
		{"@", "example.com", "@"},
		{"example.com", "example.com", "@"},
		{"tun", "example.com", "tun"},
		{"tun.example.com.", "example.com", "tun"},
		{"a.b", "example.com", "a.b"},
	} {
		if got := normName(tc[0], tc[1]); got != tc[2] {
			t.Errorf("normName(%q) = %q, want %q", tc[0], got, tc[2])
		}
	}
}
