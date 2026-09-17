package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramNotifier_SendsExpectedRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := TelegramNotifier{Token: "test-token", ChatID: "12345", BaseURL: srv.URL}
	if err := n.Notify(context.Background(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wantPath := "/bottest-token/sendMessage"; gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotBody["chat_id"] != "12345" || gotBody["text"] != "hello" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
}

func TestTelegramNotifier_ErrorsOnNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := TelegramNotifier{Token: "test-token", ChatID: "12345", BaseURL: srv.URL}
	if err := n.Notify(context.Background(), "hello"); err == nil {
		t.Fatal("expected an error for a non-2xx response, got nil")
	}
}

func TestNoopNotifier_NeverErrors(t *testing.T) {
	if err := (NoopNotifier{}).Notify(context.Background(), "hello"); err != nil {
		t.Fatalf("expected NoopNotifier to never error, got %v", err)
	}
}

func TestFromEnv_ReturnsNoopWhenUnset(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")

	if _, ok := FromEnv().(NoopNotifier); !ok {
		t.Errorf("expected NoopNotifier when env vars are unset")
	}
}

func TestFromEnv_ReturnsTelegramWhenSet(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")

	n := FromEnv()
	tg, ok := n.(TelegramNotifier)
	if !ok {
		t.Fatalf("expected TelegramNotifier, got %T", n)
	}
	if tg.Token != "tok" || tg.ChatID != "chat" {
		t.Errorf("unexpected notifier: %+v", tg)
	}
}
