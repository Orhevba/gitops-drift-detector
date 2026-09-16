// Package notify sends drift alerts to an external channel. Telegram is the
// only backend today, but Notifier is deliberately small so a Slack (or
// other webhook) implementation can be added later without touching the
// controller.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// Notifier sends a message about a drift check result.
type Notifier interface {
	Notify(ctx context.Context, message string) error
}

// NoopNotifier does nothing; used when no notifier is configured.
type NoopNotifier struct{}

func (NoopNotifier) Notify(context.Context, string) error { return nil }

// TelegramNotifier sends messages via the Telegram Bot API.
type TelegramNotifier struct {
	Token  string
	ChatID string
}

func (t TelegramNotifier) Notify(ctx context.Context, message string) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.Token)
	body, err := json.Marshal(map[string]string{
		"chat_id": t.ChatID,
		"text":    message,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram API returned status %d", resp.StatusCode)
	}
	return nil
}

// FromEnv builds a Notifier from TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID
// environment variables, or a NoopNotifier if either is unset.
func FromEnv() Notifier {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	if token == "" || chatID == "" {
		return NoopNotifier{}
	}
	return TelegramNotifier{Token: token, ChatID: chatID}
}
