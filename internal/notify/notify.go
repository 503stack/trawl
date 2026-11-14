// Package notify posts job results to a webhook (Discord or ntfy-style).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Send posts title and message to url. Discord webhooks get JSON; anything
// else (ntfy, generic) gets the message as plain text with a Title header.
func Send(ctx context.Context, url, title, msg string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var (
		body  []byte
		ctype string
	)
	if strings.Contains(url, "discord.com/api/webhooks") || strings.Contains(url, "discordapp.com/api/webhooks") {
		body, _ = json.Marshal(map[string]string{"content": "**" + title + "**\n" + msg})
		ctype = "application/json"
	} else {
		body, ctype = []byte(msg), "text/plain; charset=utf-8"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Title", title)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: HTTP %d", resp.StatusCode)
	}
	return nil
}
