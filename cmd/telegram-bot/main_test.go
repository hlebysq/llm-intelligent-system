package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

// ─── pure functions ──────────────────────────────────────────────────────────

func TestGetEnv_Default(t *testing.T) {
	result := getEnv("NONEXISTENT_BOT_VAR_9999", "fallback")
	if result != "fallback" {
		t.Errorf("expected fallback, got: %q", result)
	}
}

func TestGetEnv_SetValue(t *testing.T) {
	t.Setenv("TEST_BOT_GETENV", "hello")
	if got := getEnv("TEST_BOT_GETENV", "default"); got != "hello" {
		t.Errorf("expected hello, got: %q", got)
	}
}

func TestTruncateRunes_KeepsUTF8Valid(t *testing.T) {
	input := "Привет, мир 👋 это длинный текст"
	got := truncateRunes(input, 12)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateRunes returned invalid UTF-8: %q", got)
	}
	if got == input {
		t.Fatal("expected text to be truncated")
	}
}

func TestBuildHistoryMessage_UserReadableAndUserOnly(t *testing.T) {
	now := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	message := buildHistoryMessage(
		historySummaryView{Content: "User is exploring project context."},
		[]historyMessageView{
			{Role: "user", Content: "First user question", CreatedAt: now},
			{Role: "assistant", Content: "Assistant answer that should be hidden", CreatedAt: now},
			{Role: "user", Content: "Second user question", CreatedAt: now},
		},
		12,
	)

	checks := []string{
		"Память диалога",
		"показываю только ваши последние сообщения",
		"Ваши последние сохраненные сообщения (2 из 6)",
		"User is exploring project context.",
		"First user question",
		"Second user question",
	}
	for _, check := range checks {
		if !strings.Contains(message, check) {
			t.Fatalf("expected history message to contain %q\nFull message:\n%s", check, message)
		}
	}
	if strings.Contains(message, "Assistant answer that should be hidden") {
		t.Fatalf("assistant message should not be shown\nFull message:\n%s", message)
	}
}

func TestBuildHistoryMessage_AllowsMarkdownSpecialChars(t *testing.T) {
	now := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	message := buildHistoryMessage(
		historySummaryView{Content: "User asked food questions."},
		[]historyMessageView{
			{Role: "user", Content: "Как правильно разбивать яйцо*", CreatedAt: now},
		},
		12,
	)

	if !utf8.ValidString(message) {
		t.Fatalf("history message should be valid UTF-8: %q", message)
	}
	if !strings.Contains(message, "Как правильно разбивать яйцо*") {
		t.Fatalf("expected message to keep user text verbatim\nFull message:\n%s", message)
	}
}

// ─── authenticate ────────────────────────────────────────────────────────────

func TestAuthenticate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/login" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Проверяем, что используется сервисный аккаунт бота
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "telegram_bot" {
			t.Errorf("expected username=telegram_bot, got: %s", body["username"])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"token":      "jwt-test-token",
			"expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	if err := bot.authenticate(); err != nil {
		t.Fatalf("authenticate() returned error: %v", err)
	}
	if bot.jwtToken != "jwt-test-token" {
		t.Errorf("expected jwt-test-token, got: %q", bot.jwtToken)
	}
}

func TestAuthenticate_ServerReturns401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Invalid credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	if err := bot.authenticate(); err == nil {
		t.Error("expected error on 401, got nil")
	}
}

func TestAuthenticate_ServerUnavailable(t *testing.T) {
	bot := &TelegramBot{
		apiGatewayURL: "http://127.0.0.1:19999", // нет сервера на этом порту
		httpClient:    &http.Client{Timeout: 500 * time.Millisecond},
		logger:        zap.NewNop(),
	}

	if err := bot.authenticate(); err == nil {
		t.Error("expected error when server is unavailable")
	}
}

// ─── queryLLM ────────────────────────────────────────────────────────────────

func TestQueryLLM_Success(t *testing.T) {
	// Захватываем тело запроса, которое бот отправит в Gateway
	type captured struct{ body map[string]interface{} }
	reqCh := make(chan captured, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		reqCh <- captured{body: b}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response":           "42 is the answer.",
			"model_used":         "debate-ensemble",
			"processing_time_ms": int64(1234),
		})
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		jwtToken:      "test-token",
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	response, procTime, err := bot.queryLLM("What is life?", 123456789, "vasya", modeAnalytics)
	if err != nil {
		t.Fatalf("queryLLM returned error: %v", err)
	}
	if response != "42 is the answer." {
		t.Errorf("unexpected response: %q", response)
	}
	if procTime != 1234 {
		t.Errorf("expected procTime=1234, got %d", procTime)
	}

	cap := <-reqCh
	// telegram_id должен передаваться в теле запроса
	if cap.body["telegram_id"] != float64(123456789) {
		t.Errorf("expected telegram_id=123456789 in request body, got: %v", cap.body["telegram_id"])
	}
	if cap.body["prompt"] != "What is life?" {
		t.Errorf("unexpected prompt in request: %v", cap.body["prompt"])
	}
	if cap.body["telegram_username"] != "vasya" {
		t.Errorf("expected telegram_username=vasya, got: %v", cap.body["telegram_username"])
	}
}

func TestQueryLLM_AuthHeaderSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("expected Bearer token in Authorization header, got: %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ok", "model_used": "test", "processing_time_ms": int64(0),
		})
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		jwtToken:      "my-secret-jwt",
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	if _, _, err := bot.queryLLM("test", 1, "u", modeSimple); err != nil {
		t.Fatalf("queryLLM failed: %v", err)
	}
}

func TestQueryLLM_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		jwtToken:      "tok",
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	if _, _, err := bot.queryLLM("q", 1, "u", modeAnalytics); err == nil {
		t.Error("expected error on 500, got nil")
	}
}

func TestQueryLLM_ReAuthOnExpiredToken(t *testing.T) {
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/api/v1/auth/login" {
			// Переавторизация
			json.NewEncoder(w).Encode(map[string]string{
				"token":      "new-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}

		// Первый запрос к /query → 401
		if callCount == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"token expired"}`)
			return
		}

		// Второй запрос (после переавторизации) → 200
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "answer after reauth", "model_used": "test", "processing_time_ms": int64(10),
		})
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		jwtToken:      "expired-token",
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	resp, _, err := bot.queryLLM("hello", 1, "u", modeAnalytics)
	if err != nil {
		t.Fatalf("queryLLM failed after reauth: %v", err)
	}
	if resp != "answer after reauth" {
		t.Errorf("unexpected response: %q", resp)
	}
}

func TestHandleHistoryCommand_PassesTelegramID(t *testing.T) {
	reqCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCh <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"summary": map[string]interface{}{
				"content":       "short summary",
				"message_count": 2,
			},
			"messages": []map[string]interface{}{
				{
					"role":       "user",
					"content":    "hello",
					"created_at": time.Now().Format(time.RFC3339),
				},
			},
			"count": 1,
			"limit": 6,
		})
	}))
	defer srv.Close()

	bot := &TelegramBot{
		apiGatewayURL: srv.URL,
		jwtToken:      "test-token",
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		logger:        zap.NewNop(),
	}

	defer func() {
		_ = recover()
		query := <-reqCh
		if !strings.Contains(query, "telegram_id=777000") {
			t.Fatalf("expected telegram_id in history request query, got %q", query)
		}
	}()

	bot.handleHistoryCommandV2(777000)
}
