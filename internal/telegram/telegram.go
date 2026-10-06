// Package telegram sends BackPack+ notifications and receives bot commands.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Bot struct {
	base    string
	chatIDs []int64
	label   string
	http    *http.Client
	queue   chan string
}

// Command is a message from an allowed chat that starts with "/".
type Command struct {
	ChatID int64
	Name   string   // without the slash and any @botname suffix, lower case
	Args   []string // the rest of the words
}

func New(apiBase, token string, chatIDs []int64, label, proxy string) (*Bot, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("telegram proxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &Bot{
		base:    strings.TrimRight(apiBase, "/") + "/bot" + token,
		chatIDs: chatIDs,
		label:   label,
		http:    &http.Client{Transport: tr, Timeout: 70 * time.Second},
		queue:   make(chan string, 256),
	}, nil
}

// Notify queues an HTML message for every configured chat. It never blocks
// the caller; when Telegram is unreachable messages wait and are retried.
func (b *Bot) Notify(html string) {
	if b.label != "" {
		html = "<b>[" + Escape(b.label) + "]</b>\n" + html
	}
	select {
	case b.queue <- html:
	default:
		log.Printf("telegram: queue full, dropping message")
	}
}

// Reply sends a message to one chat right away.
func (b *Bot) Reply(ctx context.Context, chatID int64, html string) error {
	return b.send(ctx, chatID, html)
}

// RunSender delivers queued notifications until ctx ends.
func (b *Bot) RunSender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-b.queue:
			for _, id := range b.chatIDs {
				backoff := 2 * time.Second
				for attempt := 1; ; attempt++ {
					err := b.send(ctx, id, msg)
					if err == nil || ctx.Err() != nil {
						break
					}
					log.Printf("telegram: send to %d (attempt %d): %v", id, attempt, err)
					if attempt >= 8 {
						break
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
					if backoff < 60*time.Second {
						backoff *= 2
					}
				}
			}
		}
	}
}

func (b *Bot) send(ctx context.Context, chatID int64, html string) error {
	body := map[string]any{
		"chat_id":                  chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	return b.call(ctx, "sendMessage", body, nil)
}

func (b *Bot) call(ctx context.Context, method string, body any, out any) error {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/"+method, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		// The error text carries the URL, and with it the bot token.
		return fmt.Errorf("%s: request failed: %v", method, redact(err.Error(), b.base))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("%s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func redact(s, base string) string { return strings.ReplaceAll(s, base, "<telegram>") }

// RunCommands long-polls for messages and passes commands from allowed chats to handle.
func (b *Bot) RunCommands(ctx context.Context, handle func(Command)) {
	allowed := map[int64]bool{}
	for _, id := range b.chatIDs {
		allowed[id] = true
	}
	offset := int64(0)
	for ctx.Err() == nil {
		var updates []struct {
			UpdateID int64 `json:"update_id"`
			Message  *struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				From *struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Text string `json:"text"`
			} `json:"message"`
		}
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         50,
			"allowed_updates": []string{"message"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("telegram: getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || !strings.HasPrefix(m.Text, "/") {
				continue
			}
			if !allowed[m.Chat.ID] && (m.From == nil || !allowed[m.From.ID]) {
				continue
			}
			f := strings.Fields(m.Text)
			name := strings.ToLower(strings.TrimPrefix(f[0], "/"))
			if i := strings.IndexByte(name, '@'); i >= 0 {
				name = name[:i]
			}
			handle(Command{ChatID: m.Chat.ID, Name: name, Args: f[1:]})
		}
	}
}

// Escape makes text safe inside an HTML-mode message.
func Escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
