package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
	"go.uber.org/zap"

	"github.com/hlebysq/llm-intelligent-system/internal/auth"
	"github.com/hlebysq/llm-intelligent-system/internal/database"
	"github.com/hlebysq/llm-intelligent-system/internal/models"
)

// ─── helpers ────────────────────────────────────────────────────────────────

const testJWTSecret = "test-secret-key"

// newTestServer собирает Server с замоканной БД и без Redis (nil).
type fakeRateLimiter struct {
	allowed    bool
	remaining  int
	retryAfter time.Duration
	err        error
	key        string
}

func (f *fakeRateLimiter) AllowRateLimit(ctx context.Context, key string, limit int, window time.Duration) (bool, int, time.Duration, error) {
	f.key = key
	return f.allowed, f.remaining, f.retryAfter, f.err
}

func newTestServer(t *testing.T, db *sql.DB, modelProxyURL string) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	jwtManager := auth.NewJWTManager(testJWTSecret, 24*time.Hour)
	s := &Server{
		router:         gin.New(),
		db:             database.NewTestDB(db),
		cache:          nil,
		jwtManager:     jwtManager,
		logger:         zap.NewNop(),
		modelProxyURL:  modelProxyURL,
		simpleProxyURL: modelProxyURL,
	}
	s.setupRoutes()
	return s
}

// newAuthHeader генерирует Bearer-токен для тест-пользователя.
func newAuthHeader(t *testing.T, userID, username string) string {
	t.Helper()
	jwtManager := auth.NewJWTManager(testJWTSecret, 24*time.Hour)
	token, _, err := jwtManager.GenerateToken(&models.User{ID: userID, Username: username})
	if err != nil {
		t.Fatalf("failed to generate test token: %v", err)
	}
	return "Bearer " + token
}

// ─── pure functions ──────────────────────────────────────────────────────────

func TestBuildPromptWithContext_NoHistory(t *testing.T) {
	result := buildPromptWithContext("", nil, "What is AI?")
	if result != "What is AI?" {
		t.Errorf("expected unchanged prompt, got: %q", result)
	}
}

func TestBuildPromptWithContext_EmptyHistory(t *testing.T) {
	result := buildPromptWithContext("", []models.ChatMessage{}, "Hello")
	if result != "Hello" {
		t.Errorf("empty history should return original prompt, got: %q", result)
	}
}

func TestBuildPromptWithContext_ContainsHistory(t *testing.T) {
	history := []models.ChatMessage{
		{Role: "user", Content: "What is ML?"},
		{Role: "assistant", Content: "ML is machine learning."},
	}
	result := buildPromptWithContext("", history, "Give me an example.")

	checks := []string{
		"What is ML?",
		"ML is machine learning.",
		"Give me an example.",
		"Пользователь:",
		"Ассистент:",
	}
	for _, s := range checks {
		if !strings.Contains(result, s) {
			t.Errorf("result should contain %q\nFull result:\n%s", s, result)
		}
	}
}

func TestBuildPromptWithContext_NewPromptAtEnd(t *testing.T) {
	history := []models.ChatMessage{
		{Role: "user", Content: "old question"},
	}
	result := buildPromptWithContext("", history, "new question")

	oldIdx := strings.Index(result, "old question")
	newIdx := strings.Index(result, "new question")
	if oldIdx == -1 || newIdx == -1 {
		t.Fatal("both messages must be present")
	}
	if newIdx < oldIdx {
		t.Error("new question should appear after old question")
	}
}

func TestBuildPromptWithContext_IncludesSummary(t *testing.T) {
	result := buildPromptWithContext("User prefers concise Go explanations.", nil, "What next?")

	checks := []string{
		"Conversation summary:",
		"User prefers concise Go explanations.",
		"What next?",
	}
	for _, s := range checks {
		if !strings.Contains(result, s) {
			t.Errorf("result should contain %q\nFull result:\n%s", s, result)
		}
	}
}

func TestGetEnv_Default(t *testing.T) {
	result := getEnv("NONEXISTENT_VAR_GATEWAY_9999", "fallback")
	if result != "fallback" {
		t.Errorf("expected fallback, got: %q", result)
	}
}

func TestGetEnv_SetValue(t *testing.T) {
	t.Setenv("TEST_GETENV_GATEWAY", "custom")
	if got := getEnv("TEST_GETENV_GATEWAY", "default"); got != "custom" {
		t.Errorf("expected custom, got: %q", got)
	}
}

// ─── auth middleware ─────────────────────────────────────────────────────────

func TestOpenAPISpecRoute(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if contentType := w.Header().Get("Content-Type"); !strings.Contains(contentType, "application/yaml") {
		t.Fatalf("expected yaml content type, got %q", contentType)
	}
	if !strings.Contains(w.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("expected embedded OpenAPI document, got: %s", w.Body.String())
	}
}

func TestSwaggerUIRoute(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if contentType := w.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("expected html content type, got %q", contentType)
	}
	body := w.Body.String()
	if !strings.Contains(body, "SwaggerUIBundle") {
		t.Fatalf("expected Swagger UI page, got: %s", body)
	}
	if !strings.Contains(body, "/openapi.yaml") {
		t.Fatalf("expected Swagger UI to load embedded spec, got: %s", body)
	}
}

func TestOpenAPIDocumentParses(t *testing.T) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal(apiGatewayOpenAPISpec, &doc); err != nil {
		t.Fatalf("embedded OpenAPI document should be valid YAML: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Fatalf("unexpected OpenAPI version: %v", doc["openapi"])
	}
	if _, ok := doc["paths"].(map[string]interface{}); !ok {
		t.Fatal("OpenAPI document should define paths")
	}
	if _, ok := doc["components"].(map[string]interface{}); !ok {
		t.Fatal("OpenAPI document should define components")
	}
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not.a.valid.token")
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	// Минимальный мок для handleQuery: getChatHistory + 2×saveChatMessage + logQuery
	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "user_id", "role", "content", "created_at"},
		))
	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO query_logs").WillReturnResult(sqlmock.NewResult(1, 1))

	// Заглушка оркестратора
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ok", "model": "test", "processing_time": 1,
		})
	}))
	defer orch.Close()

	s := newTestServer(t, db, orch.URL+"/api/v1/generate")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", newAuthHeader(t, "uid-1", "testuser"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid token, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── login handler ───────────────────────────────────────────────────────────

func TestHandleQuery_RateLimitExceeded(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	limiter := &fakeRateLimiter{allowed: false, remaining: 0, retryAfter: 30 * time.Second}
	s := newTestServer(t, db, "")
	s.rateLimiter = limiter
	s.queryLimit = 2
	s.queryWindow = time.Minute

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", newAuthHeader(t, "uid-rate", "testuser"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
	if limiter.key != "rate_limit:query:uid-rate" {
		t.Fatalf("unexpected rate limit key: %q", limiter.key)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("expected Retry-After=30, got %q", got)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "2" {
		t.Fatalf("expected X-RateLimit-Limit=2, got %q", got)
	}
}

func TestHandleHistory_ReturnsSummaryAndRecentMessages(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	userID := "uid-history"
	updatedAt := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)

	mock.ExpectQuery("SELECT user_id, summary, message_count, updated_at").
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "summary", "message_count", "updated_at"}).
			AddRow(userID, "User prefers short answers about Go.", 12, updatedAt))

	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WithArgs(userID, chatContextMessageLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "role", "content", "created_at"}).
			AddRow("m1", userID, "user", "What is context?", updatedAt.Add(-time.Minute)).
			AddRow("m2", userID, "assistant", "It is compact memory plus recent messages.", updatedAt))

	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil)
	req.Header.Set("Authorization", newAuthHeader(t, userID, "testuser"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp models.ChatHistoryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Summary.Content != "User prefers short answers about Go." {
		t.Fatalf("unexpected summary: %q", resp.Summary.Content)
	}
	if resp.Summary.MessageCount != 12 {
		t.Fatalf("unexpected summary message count: %d", resp.Summary.MessageCount)
	}
	if resp.Summary.UpdatedAt == nil || !resp.Summary.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("unexpected summary updated_at: %v", resp.Summary.UpdatedAt)
	}
	if resp.Count != 2 || resp.Limit != chatContextMessageLimit {
		t.Fatalf("unexpected count/limit: count=%d limit=%d", resp.Count, resp.Limit)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resp.Messages))
	}
	if resp.Messages[0].Role != "user" || resp.Messages[0].Content != "What is context?" {
		t.Fatalf("unexpected first message: %+v", resp.Messages[0])
	}
	if resp.Messages[1].Role != "assistant" {
		t.Fatalf("unexpected second message: %+v", resp.Messages[1])
	}
}

func TestHandleHistory_ResolvesTelegramUser(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	serviceUID := "service-uid"
	tgUID := "tg-history-user"

	mock.ExpectQuery("SELECT id FROM users WHERE telegram_id").
		WithArgs(int64(777000)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(tgUID))
	mock.ExpectQuery("SELECT user_id, summary, message_count, updated_at").
		WithArgs(tgUID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WithArgs(tgUID, chatContextMessageLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "role", "content", "created_at"}).
			AddRow("m1", tgUID, "user", "telegram question", time.Now()))

	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/history?telegram_id=777000", nil)
	req.Header.Set("Authorization", newAuthHeader(t, serviceUID, "telegram_bot"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp models.ChatHistoryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].UserID != tgUID {
		t.Fatalf("expected telegram user's messages, got: %+v", resp.Messages)
	}
}

func TestHandleLogin_InvalidJSON(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := newTestServer(t, db, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		bytes.NewBufferString(`{invalid}`))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleLogin_UserNotFound(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	mock.ExpectQuery("SELECT id, username, email, password_hash, created_at, updated_at").
		WithArgs("unknown").
		WillReturnError(sql.ErrNoRows)

	s := newTestServer(t, db, "")
	body := `{"username":"unknown","password":"pass"}`

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleLogin_WrongPassword(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	// Хэш от "password123"
	hash := "$2a$10$t.xd7zn2hWpx93DMNl6G8ep2zNuP0j.u64Z3t7YhVEbJioO2121DS"
	rows := sqlmock.NewRows(
		[]string{"id", "username", "email", "password_hash", "created_at", "updated_at"}).
		AddRow("uid-1", "testuser", "t@t.com", hash, time.Now(), time.Now())

	mock.ExpectQuery("SELECT id, username, email, password_hash, created_at, updated_at").
		WithArgs("testuser").
		WillReturnRows(rows)

	s := newTestServer(t, db, "")
	body := `{"username":"testuser","password":"wrongpassword"}`

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleLogin_Success(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	hash := "$2a$10$t.xd7zn2hWpx93DMNl6G8ep2zNuP0j.u64Z3t7YhVEbJioO2121DS"
	rows := sqlmock.NewRows(
		[]string{"id", "username", "email", "password_hash", "created_at", "updated_at"}).
		AddRow("uid-1", "testuser", "t@t.com", hash, time.Now(), time.Now())

	mock.ExpectQuery("SELECT id, username, email, password_hash, created_at, updated_at").
		WithArgs("testuser").
		WillReturnRows(rows)

	s := newTestServer(t, db, "")
	body := `{"username":"testuser","password":"password123"}`

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp models.LoginResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Token == "" {
		t.Error("expected non-empty JWT token in response")
	}
}

// ─── query handler ───────────────────────────────────────────────────────────

func TestHandleQuery_Success(t *testing.T) {
	// Заглушка debate-orchestrator
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response":        "The answer is 42.",
			"model":           "debate-ensemble",
			"processing_time": 1500,
		})
	}))
	defer orch.Close()

	db, mock, _ := sqlmock.New()
	defer db.Close()
	userID := "user-uuid-test"

	// getChatHistory → пустой результат
	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WithArgs(userID, 10).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "user_id", "role", "content", "created_at"}))

	// saveChatMessage x2
	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))

	// logQuery
	mock.ExpectExec("INSERT INTO query_logs").WillReturnResult(sqlmock.NewResult(1, 1))

	s := newTestServer(t, db, orch.URL+"/api/v1/generate")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"What is the answer?"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", newAuthHeader(t, userID, "testuser"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp models.QueryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Response != "The answer is 42." {
		t.Errorf("unexpected response: %q", resp.Response)
	}
}

func TestHandleQuery_ContextInjectedIntoPrompt(t *testing.T) {
	// Сохраняем тело запроса, пришедшего в оркестратор
	type orchRequest struct{ body map[string]interface{} }
	reqCh := make(chan orchRequest, 2)

	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		reqCh <- orchRequest{body: b}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "follow-up answer", "model": "test", "processing_time": 1,
		})
	}))
	defer orch.Close()

	db, mock, _ := sqlmock.New()
	defer db.Close()
	userID := "uid-ctx"

	// getChatHistory → один предыдущий обмен
	histRows := sqlmock.NewRows([]string{"id", "user_id", "role", "content", "created_at"}).
		AddRow("m1", userID, "user", "What is Go?", time.Now().Add(-time.Hour)).
		AddRow("m2", userID, "assistant", "Go is a programming language.", time.Now().Add(-time.Minute))
	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WillReturnRows(histRows)

	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO query_logs").WillReturnResult(sqlmock.NewResult(1, 1))

	s := newTestServer(t, db, orch.URL+"/api/v1/generate")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"Who created it?"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", newAuthHeader(t, userID, "u"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Проверяем, что в оркестратор ушёл обогащённый промпт с историей
	captured := <-reqCh
	sentPrompt, _ := captured.body["prompt"].(string)
	if !strings.Contains(sentPrompt, "What is Go?") {
		t.Error("orchestrator should receive prompt with history context")
	}
	if !strings.Contains(sentPrompt, "Who created it?") {
		t.Error("orchestrator should receive current question")
	}
}

func TestHandleQuery_OrchestratorError(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer orch.Close()

	db, mock, _ := sqlmock.New()
	defer db.Close()
	userID := "uid-err"

	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "role", "content", "created_at"}))
	mock.ExpectExec("INSERT INTO query_logs").WillReturnResult(sqlmock.NewResult(1, 1))

	s := newTestServer(t, db, orch.URL+"/api/v1/generate")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(`{"prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", newAuthHeader(t, userID, "u"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestHandleQuery_TelegramUserCreated(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "hello tg user", "model": "test", "processing_time": 1,
		})
	}))
	defer orch.Close()

	db, mock, _ := sqlmock.New()
	defer db.Close()

	serviceUID := "service-uid"
	tgUID := "tg-user-uuid"

	// findOrCreateTelegramUser: пользователь уже существует
	mock.ExpectQuery("SELECT id FROM users WHERE telegram_id").
		WithArgs(int64(777000)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(tgUID))

	// getChatHistory для tgUID
	mock.ExpectQuery("SELECT id, user_id, role, content, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "role", "content", "created_at"}))

	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO chat_messages").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO query_logs").WillReturnResult(sqlmock.NewResult(1, 1))

	s := newTestServer(t, db, orch.URL+"/api/v1/generate")

	body := `{"prompt":"hi","telegram_id":777000,"telegram_username":"vasya"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", newAuthHeader(t, serviceUID, "telegram_bot"))
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
