package models

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// User представляет пользователя системы
type User struct {
	ID           string    `json:"id" db:"id"`
	Username     string    `json:"username" db:"username"`
	Email        string    `json:"email" db:"email"`
	PasswordHash string    `json:"-" db:"password_hash"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// QueryRequest представляет входящий запрос к LLM
type QueryRequest struct {
	Prompt         string `json:"prompt" binding:"required"`
	PreferredModel string `json:"preferred_model,omitempty"`
	TimeoutMS      int    `json:"timeout_ms,omitempty"`
	MaxTokens      int    `json:"max_tokens,omitempty"`
}

// QueryResponse представляет ответ системы
type QueryResponse struct {
	Response       string `json:"response"`
	ModelUsed      string `json:"model_used"`
	ProcessingTime int64  `json:"processing_time_ms"`
	TokensUsed     int    `json:"tokens_used,omitempty"`
}

// ErrorResponse представляет ответ с ошибкой
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Details string `json:"details,omitempty"`
}

// LoginRequest представляет запрос на авторизацию
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResponse представляет ответ при авторизации
type LoginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      User      `json:"user"`
}

// JWTClaims представляет claims для JWT токена
type JWTClaims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// QueryLog представляет запись в логах запросов
type QueryLog struct {
	ID            string    `json:"id" db:"id"`
	UserID        *string   `json:"user_id" db:"user_id"`
	OriginalQuery string    `json:"original_query" db:"original_query"`
	ModelUsed     string    `json:"model_used" db:"model_used"`
	ResponseText  string    `json:"response_text" db:"response_text"`
	LatencyMS     int       `json:"latency_ms" db:"latency_ms"`
	Status        string    `json:"status" db:"status"`
	ErrorMessage  *string   `json:"error_message,omitempty" db:"error_message"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
}

// OllamaGenerateRequest представляет запрос к Ollama API
type OllamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// OllamaGenerateResponse представляет ответ от Ollama API
type OllamaGenerateResponse struct {
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	Response  string    `json:"response"`
	Done      bool      `json:"done"`
}

// HealthResponse представляет ответ health check
type HealthResponse struct {
	Status    string            `json:"status"`
	Service   string            `json:"service"`
	Timestamp time.Time         `json:"timestamp"`
	Checks    map[string]string `json:"checks,omitempty"`
}
