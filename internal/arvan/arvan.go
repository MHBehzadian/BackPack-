// Package arvan is a minimal ArvanCloud CDN API (v4.0) client for reading and
// re-pointing a single A record.
package arvan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Base   string
	APIKey string
	Domain string
	HTTP   *http.Client
}

func New(base, apiKey, domain, proxy string) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("arvan proxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &Client{
		Base:   strings.TrimRight(base, "/"),
		APIKey: apiKey,
		Domain: domain,
		HTTP:   &http.Client{Transport: tr, Timeout: 20 * time.Second},
	}, nil
}

// Record is the part of an Arvan DNS record BackPack+ cares about. Raw keeps
// every field Arvan returned so an update can send the record back unchanged
// except for its IP.
type Record struct {
	ID    string
	Name  string
	Type  string
	IPs   []string
	TTL   int
	Cloud bool
	Raw   map[string]json.RawMessage
}

type envelope struct {
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Errors  json.RawMessage `json:"errors"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(c.APIKey)
	if !strings.HasPrefix(strings.ToLower(key), "apikey ") {
		key = "Apikey " + key
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var env envelope
	jerr := json.Unmarshal(raw, &env)
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		if len(env.Errors) > 0 && string(env.Errors) != "null" {
			msg += " " + string(env.Errors)
		}
		return fmt.Errorf("arvan %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if jerr != nil {
		return fmt.Errorf("arvan %s %s: bad response: %w", method, path, jerr)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("arvan %s %s: decode data: %w", method, path, err)
		}
	}
	return nil
}

func parseRecord(raw map[string]json.RawMessage) (Record, error) {
	r := Record{Raw: raw}
	str := func(k string) string {
		var s string
		json.Unmarshal(raw[k], &s)
		return s
	}
	r.ID, r.Name, r.Type = str("id"), str("name"), strings.ToLower(str("type"))
	json.Unmarshal(raw["ttl"], &r.TTL)
	json.Unmarshal(raw["cloud"], &r.Cloud)
	if r.Type == "a" {
		var vals []struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(raw["value"], &vals); err != nil {
			return r, fmt.Errorf("record %s: unexpected value: %w", r.Name, err)
		}
		for _, v := range vals {
			r.IPs = append(r.IPs, v.IP)
		}
	}
	return r, nil
}

// FindA finds the A record with the given name ("@" for the apex).
func (c *Client) FindA(ctx context.Context, name string) (Record, error) {
	want := normName(name, c.Domain)
	for page := 1; page <= 50; page++ {
		var list []map[string]json.RawMessage
		path := fmt.Sprintf("/domains/%s/dns-records?page=%d&per_page=100", url.PathEscape(c.Domain), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return Record{}, err
		}
		for _, raw := range list {
			r, err := parseRecord(raw)
			if err != nil || r.Type != "a" {
				continue
			}
			if normName(r.Name, c.Domain) == want {
				return r, nil
			}
		}
		if len(list) < 100 {
			break
		}
	}
	return Record{}, fmt.Errorf("no A record %q found in zone %s", name, c.Domain)
}

// Get fetches a record by id.
func (c *Client) Get(ctx context.Context, id string) (Record, error) {
	var raw map[string]json.RawMessage
	path := fmt.Sprintf("/domains/%s/dns-records/%s", url.PathEscape(c.Domain), url.PathEscape(id))
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return Record{}, err
	}
	return parseRecord(raw)
}

// SetIP re-points the A record to a single IP, keeping its other settings
// (TTL, cloud/CDN flag, upstream, ip filter mode, port/weight of the value).
func (c *Client) SetIP(ctx context.Context, cur Record, ip string) (Record, error) {
	if cur.ID == "" {
		return Record{}, errors.New("record has no id")
	}
	body := map[string]any{}
	for _, k := range []string{"type", "name", "ttl", "cloud", "upstream_https", "ip_filter_mode"} {
		if v, ok := cur.Raw[k]; ok && string(v) != "null" {
			body[k] = v
		}
	}
	body["type"] = "a"
	body["name"] = cur.Name

	val := map[string]json.RawMessage{}
	var old []map[string]json.RawMessage
	if json.Unmarshal(cur.Raw["value"], &old) == nil && len(old) > 0 {
		for k, v := range old[0] {
			if k == "port" || k == "weight" || k == "country" {
				if string(v) != "null" {
					val[k] = v
				}
			}
		}
	}
	ipJSON, _ := json.Marshal(ip)
	val["ip"] = ipJSON
	body["value"] = []any{val}

	var raw map[string]json.RawMessage
	path := fmt.Sprintf("/domains/%s/dns-records/%s", url.PathEscape(c.Domain), url.PathEscape(cur.ID))
	if err := c.do(ctx, http.MethodPut, path, body, &raw); err != nil {
		return Record{}, err
	}
	if len(raw) == 0 {
		return c.Get(ctx, cur.ID)
	}
	return parseRecord(raw)
}

func normName(name, domain string) string {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	d := strings.ToLower(strings.TrimSuffix(domain, "."))
	if n == "" || n == d {
		return "@"
	}
	return strings.TrimSuffix(n, "."+d)
}
