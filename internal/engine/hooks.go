package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/phantomwar/ad-postgres-backup/internal/config"
)

type webhookPayload struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message,omitempty"`
}

// notifyHooks runs executable scripts in HOOKS_DIR and dispatches JSON webhooks.
func notifyHooks(ctx context.Context, logger *slog.Logger, cfg *config.Config, action string, detail string) error {
	if cfg.HooksDir != "" {
		if err := runHooksDir(ctx, cfg.HooksDir, action); err != nil {
			return err
		}
	}

	var urls []string
	switch action {
	case "pre-backup":
		if cfg.WebhookPreBackupURL != "" {
			urls = append(urls, cfg.WebhookPreBackupURL)
		}
	case "post-backup":
		if cfg.WebhookURL != "" {
			urls = append(urls, cfg.WebhookURL)
		}
		if cfg.WebhookPostBackupURL != "" {
			urls = append(urls, cfg.WebhookPostBackupURL)
		}
	case "error":
		if cfg.WebhookURL != "" {
			urls = append(urls, cfg.WebhookURL)
		}
		if cfg.WebhookErrorURL != "" {
			urls = append(urls, cfg.WebhookErrorURL)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	body, _ := json.Marshal(webhookPayload{
		Status:    action,
		Timestamp: time.Now().UTC(),
		Message:   detail,
	})

	for _, u := range urls {
		if err := postWebhook(ctx, client, u, body); err != nil {
			logger.Warn("webhook delivery failed", "action", action, "url", u, "error", err)
			if action == "pre-backup" {
				return fmt.Errorf("pre-backup webhook %s: %w", u, err)
			}
		}
	}
	return nil
}

func runHooksDir(ctx context.Context, dir, action string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read HOOKS_DIR %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if action == "post-backup" {
		for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
			names[i], names[j] = names[j], names[i]
		}
	}
	for _, n := range names {
		full := filepath.Join(dir, n)
		cmd := exec.CommandContext(ctx, full, action)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("hook %s (%s): %w: %s", n, action, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func postWebhook(ctx context.Context, client *http.Client, targetURL string, payload []byte) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return lastErr
}
