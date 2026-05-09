package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/hlebysq/llm-intelligent-system/internal/auth"
	"github.com/hlebysq/llm-intelligent-system/internal/cache"
	"github.com/hlebysq/llm-intelligent-system/internal/database"
	"github.com/hlebysq/llm-intelligent-system/internal/logger"
	"github.com/hlebysq/llm-intelligent-system/internal/models"
)

type Server struct {
	router         *gin.Engine
	db             *database.DB
	cache          *cache.RedisClient
	rateLimiter    rateLimiter
	jwtManager     *auth.JWTManager
	logger         *zap.Logger
	modelProxyURL  string // debate-orchestrator /api/v1/generate (аналитический режим)
	simpleProxyURL string // debate-orchestrator /api/v1/simple    (обычный режим)
	queryLimit     int
	queryWindow    time.Duration
}

type rateLimiter interface {
	AllowRateLimit(ctx context.Context, key string, limit int, window time.Duration) (bool, int, time.Duration, error)
}

const chatContextMessageLimit = 12

//go:embed docs/api-gateway/openapi.yaml
var apiGatewayOpenAPISpec []byte

//go:embed docs/api-gateway/swagger-ui.html
var apiGatewaySwaggerUI []byte

func main() {
	// Загрузка переменных окружения
	_ = godotenv.Load()

	// Инициализация логгера
	env := getEnv("ENVIRONMENT", "development")
	if err := logger.InitLogger(env); err != nil {
		panic(fmt.Sprintf("Failed to initialize logger: %v", err))
	}
	defer logger.Sync()

	log := logger.GetLogger()
	log.Info("Starting API Gateway", zap.String("environment", env))

	// Инициализация базы данных
	dbConfig := database.Config{
		Host:     getEnv("DB_HOST", "localhost"),
		Port:     getEnv("DB_PORT", "5432"),
		User:     getEnv("DB_USER", "llm_user"),
		Password: getEnv("DB_PASSWORD", "password"),
		DBName:   getEnv("DB_NAME", "llm_system"),
	}

	db, err := database.NewPostgresDB(dbConfig, log)
	if err != nil {
		log.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer db.Close()

	// Инициализация Redis
	redisConfig := cache.Config{
		Host:     getEnv("REDIS_HOST", "localhost"),
		Port:     getEnv("REDIS_PORT", "6379"),
		Password: getEnv("REDIS_PASSWORD", ""),
		DB:       0,
	}

	redisClient, err := cache.NewRedisClient(redisConfig, log)
	if err != nil {
		log.Fatal("Failed to connect to Redis", zap.Error(err))
	}
	defer redisClient.Close()

	// Инициализация JWT менеджера
	jwtSecret := getEnv("JWT_SECRET", "your-secret-key")
	jwtManager := auth.NewJWTManager(jwtSecret, 24*time.Hour)
	queryLimit := getEnvInt("QUERY_RATE_LIMIT_REQUESTS", 20)
	queryWindow := time.Duration(getEnvInt("QUERY_RATE_LIMIT_WINDOW_SECONDS", 60)) * time.Second

	// Создание сервера
	server := &Server{
		router:         gin.Default(),
		db:             db,
		cache:          redisClient,
		rateLimiter:    redisClient,
		jwtManager:     jwtManager,
		logger:         log,
		modelProxyURL:  getEnv("DEBATE_ORCHESTRATOR_ANALYTICS_URL", "http://debate-orchestrator:8082/api/v1/generate"),
		simpleProxyURL: getEnv("DEBATE_ORCHESTRATOR_SIMPLE_URL", "http://debate-orchestrator:8082/api/v1/simple"),
		queryLimit:     queryLimit,
		queryWindow:    queryWindow,
	}

	// Настройка маршрутов
	server.setupRoutes()

	// Запуск HTTP сервера
	port := getEnv("API_GATEWAY_PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      server.router,
		ReadTimeout:  600 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		log.Info("API Gateway started", zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("Failed to start server", zap.Error(err))
		}
	}()

	// Ожидание сигнала завершения
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown", zap.Error(err))
	}

	log.Info("Server exited")
}

func (s *Server) setupRoutes() {
	s.router.GET("/openapi.yaml", s.handleOpenAPISpec)
	s.router.GET("/docs", s.handleSwaggerUI)
	s.router.GET("/docs/", s.handleSwaggerUI)
	// Middleware
	s.router.Use(gin.Recovery())
	s.router.Use(s.corsMiddleware())

	// Публичные эндпоинты
	public := s.router.Group("/api/v1")
	{
		public.POST("/auth/login", s.handleLogin)
		public.GET("/health", s.handleHealth)
	}

	// Защищенные эндпоинты
	protected := s.router.Group("/api/v1")
	protected.Use(s.authMiddleware())
	{
		protected.POST("/query", s.handleQuery)
		protected.GET("/history", s.handleHistory)
		protected.PUT("/users/mode", s.handleSetMode)
		protected.GET("/users/mode", s.handleGetMode)
	}
}

// Middleware для CORS
func (s *Server) handleOpenAPISpec(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", apiGatewayOpenAPISpec)
}

func (s *Server) handleSwaggerUI(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", apiGatewaySwaggerUI)
}

func (s *Server) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// Middleware для JWT аутентификации
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, models.ErrorResponse{
				Error: "Authorization header required",
				Code:  http.StatusUnauthorized,
			})
			c.Abort()
			return
		}

		// Формат: "Bearer <token>"
		tokenString := authHeader
		if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			tokenString = authHeader[7:]
		}

		claims, err := s.jwtManager.ValidateToken(tokenString)
		if err != nil {
			c.JSON(http.StatusUnauthorized, models.ErrorResponse{
				Error:   "Invalid token",
				Code:    http.StatusUnauthorized,
				Details: err.Error(),
			})
			c.Abort()
			return
		}

		// Сохраняем информацию о пользователе в контексте
		c.Set("user_id", claims.UserID)
		c.Set("username", claims.Username)
		c.Next()
	}
}

// Handler для авторизации
func (s *Server) checkQueryRateLimit(c *gin.Context, userID string) bool {
	if s.rateLimiter == nil || s.queryLimit <= 0 || s.queryWindow <= 0 {
		return true
	}

	key := "rate_limit:query:" + userID
	allowed, remaining, retryAfter, err := s.rateLimiter.AllowRateLimit(c.Request.Context(), key, s.queryLimit, s.queryWindow)
	if err != nil {
		s.logger.Warn("Failed to check query rate limit", zap.Error(err), zap.String("user_id", userID))
		return true
	}

	c.Header("X-RateLimit-Limit", strconv.Itoa(s.queryLimit))
	c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(retryAfter).Unix(), 10))

	if allowed {
		return true
	}

	retryAfterSeconds := int(retryAfter.Seconds())
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
	c.JSON(http.StatusTooManyRequests, models.ErrorResponse{
		Error:   "Rate limit exceeded",
		Code:    http.StatusTooManyRequests,
		Details: fmt.Sprintf("try again in %d seconds", retryAfterSeconds),
	})
	return false
}

func (s *Server) handleLogin(c *gin.Context) {
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error:   "Invalid request body",
			Code:    http.StatusBadRequest,
			Details: err.Error(),
		})
		return
	}

	var user models.User
	query := `SELECT id, username, email, password_hash, created_at, updated_at 
	          FROM users WHERE username = $1`
	err := s.db.QueryRow(query, req.Username).Scan(
		&user.ID, &user.Username, &user.Email, &user.PasswordHash,
		&user.CreatedAt, &user.UpdatedAt,
	)

	if err != nil {
		s.logger.Warn("Login failed: user not found",
			zap.String("username", req.Username),
		)
		c.JSON(http.StatusUnauthorized, models.ErrorResponse{
			Error: "Invalid credentials",
			Code:  http.StatusUnauthorized,
		})
		return
	}

	// Проверка пароля
	if err := bcrypt.CompareHashAndPassword(
		[]byte(user.PasswordHash),
		[]byte(req.Password),
	); err != nil {
		s.logger.Warn("Login failed: incorrect password",
			zap.String("username", req.Username),
		)
		c.JSON(http.StatusUnauthorized, models.ErrorResponse{
			Error: "Invalid credentials",
			Code:  http.StatusUnauthorized,
		})
		return
	}

	// Генерация JWT токена
	token, expiresAt, err := s.jwtManager.GenerateToken(&user)
	if err != nil {
		s.logger.Error("Failed to generate token", zap.Error(err))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error: "Failed to generate token",
			Code:  http.StatusInternalServerError,
		})
		return
	}

	s.logger.Info("User logged in successfully",
		zap.String("user_id", user.ID),
		zap.String("username", user.Username),
	)

	c.JSON(http.StatusOK, models.LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		User:      user,
	})
}

// Handler для обработки запросов к LLM
func (s *Server) handleQuery(c *gin.Context) {
	serviceUserID, _ := c.Get("user_id")

	var req models.QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error:   "Invalid request body",
			Code:    http.StatusBadRequest,
			Details: err.Error(),
		})
		return
	}

	// Определяем реального пользователя: если бот передал telegram_id,
	// находим или создаём запись в БД; иначе — используем аккаунт из JWT.
	actualUserID := serviceUserID.(string)
	if req.TelegramID != 0 {
		tgUserID, err := s.findOrCreateTelegramUser(req.TelegramID, req.TelegramUsername)
		if err != nil {
			s.logger.Error("Failed to resolve Telegram user",
				zap.Error(err), zap.Int64("telegram_id", req.TelegramID))
		} else {
			actualUserID = tgUserID
		}
	}

	if !s.checkQueryRateLimit(c, actualUserID) {
		return
	}

	s.logger.Info("Processing query",
		zap.String("user_id", actualUserID),
		zap.Int64("telegram_id", req.TelegramID),
		zap.String("prompt", req.Prompt[:min(50, len(req.Prompt))]),
	)

	startTime := time.Now()

	// Получаем историю диалога и строим обогащённый промпт
	summary, err := s.getChatSummary(actualUserID)
	if err != nil {
		s.logger.Warn("Failed to fetch chat summary", zap.Error(err))
	}
	history, err := s.getChatHistory(actualUserID, chatContextMessageLimit)
	if err != nil {
		s.logger.Warn("Failed to fetch chat history", zap.Error(err))
	}
	enrichedPrompt := buildPromptWithContext(summary, history, req.Prompt)

	// Выбираем эндпоинт в зависимости от режима
	proxyURL := s.modelProxyURL
	if req.Mode == "simple" {
		proxyURL = s.simpleProxyURL
	}

	response, modelUsed, err := s.callModelProxy(proxyURL, enrichedPrompt)
	if err != nil {
		s.logger.Error("Failed to call debate orchestrator", zap.Error(err))
		s.logQuery(actualUserID, req.Prompt, "", "", 0, "error", err.Error())
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to process query",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}

	processingTime := time.Since(startTime).Milliseconds()

	// Сохраняем обмен в историю чата (оригинальный промпт, не обогащённый)
	s.saveChatMessage(actualUserID, "user", req.Prompt)
	s.saveChatMessage(actualUserID, "assistant", response)

	s.logQuery(actualUserID, req.Prompt, modelUsed, response,
		int(processingTime), "success", "")

	if err := s.updateChatSummary(actualUserID, summary, req.Prompt, response); err != nil {
		s.logger.Warn("Failed to update chat summary", zap.Error(err), zap.String("user_id", actualUserID))
	}

	c.JSON(http.StatusOK, models.QueryResponse{
		Response:       response,
		ModelUsed:      modelUsed,
		ProcessingTime: processingTime,
	})
}

// Handler для получения истории запросов
func (s *Server) handleHistory(c *gin.Context) {
	userID, _ := c.Get("user_id")
	actualUserID := userID.(string)
	if telegramID := c.Query("telegram_id"); telegramID != "" {
		parsedTelegramID, err := strconv.ParseInt(telegramID, 10, 64)
		if err != nil || parsedTelegramID <= 0 {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{
				Error: "telegram_id must be a positive integer",
				Code:  http.StatusBadRequest,
			})
			return
		}

		tgUserID, err := s.findOrCreateTelegramUser(parsedTelegramID, "")
		if err != nil {
			s.logger.Error("Failed to resolve Telegram user",
				zap.Error(err), zap.Int64("telegram_id", parsedTelegramID))
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{
				Error: "Failed to fetch history",
				Code:  http.StatusInternalServerError,
			})
			return
		}
		actualUserID = tgUserID
	}

	summary, err := s.getChatSummaryDetails(actualUserID)
	if err != nil {
		s.logger.Error("Failed to fetch chat summary", zap.Error(err))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error: "Failed to fetch history",
			Code:  http.StatusInternalServerError,
		})
		return
	}

	messages, err := s.getChatHistory(actualUserID, chatContextMessageLimit)
	if err != nil {
		s.logger.Error("Failed to fetch chat messages", zap.Error(err))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error: "Failed to fetch history",
			Code:  http.StatusInternalServerError,
		})
		return
	}

	var updatedAt *time.Time
	if !summary.UpdatedAt.IsZero() {
		updatedAt = &summary.UpdatedAt
	}

	c.JSON(http.StatusOK, models.ChatHistoryResponse{
		Summary: models.ChatSummaryResponse{
			Content:      summary.Summary,
			MessageCount: summary.MessageCount,
			UpdatedAt:    updatedAt,
		},
		Messages: messages,
		Count:    len(messages),
		Limit:    chatContextMessageLimit,
	})
}

// Handler для health check
func (s *Server) handleHealth(c *gin.Context) {
	checks := make(map[string]string)

	// Проверка PostgreSQL
	if err := s.db.HealthCheck(); err != nil {
		checks["postgres"] = "unhealthy: " + err.Error()
	} else {
		checks["postgres"] = "healthy"
	}

	// Проверка Redis
	if s.cache == nil {
		checks["redis"] = "disabled"
	} else if err := s.cache.HealthCheck(); err != nil {
		checks["redis"] = "unhealthy: " + err.Error()
	} else {
		checks["redis"] = "healthy"
	}

	status := "healthy"
	statusCode := http.StatusOK

	for _, check := range checks {
		if check != "healthy" {
			status = "unhealthy"
			statusCode = http.StatusServiceUnavailable
			break
		}
	}

	c.JSON(statusCode, models.HealthResponse{
		Status:    status,
		Service:   "api-gateway",
		Timestamp: time.Now(),
		Checks:    checks,
	})
}

// Вспомогательная функция для вызова debate orchestrator.
// Возвращает (response, modelUsed, error).
func (s *Server) callModelProxy(url, prompt string) (string, string, error) {
	client := &http.Client{
		Timeout: 540 * time.Second,
	}

	requestBody := map[string]interface{}{
		"prompt": prompt,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return "", "", err
	}

	resp, err := client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("orchestrator returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}

	response, ok := result["response"].(string)
	if !ok {
		return "", "", fmt.Errorf("invalid response format")
	}

	modelUsed, _ := result["model"].(string)
	if modelUsed == "" {
		modelUsed = "unknown"
	}

	return response, modelUsed, nil
}

// Логирование запроса в БД
func (s *Server) logQuery(userID, query, model, response string, latencyMS int, status, errorMsg string) {
	var errorMsgPtr *string
	if errorMsg != "" {
		errorMsgPtr = &errorMsg
	}

	insertQuery := `
		INSERT INTO query_logs (user_id, original_query, model_used, response_text, 
		                       latency_ms, status, error_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	_, err := s.db.Exec(insertQuery, userID, query, model, response,
		latencyMS, status, errorMsgPtr)
	if err != nil {
		s.logger.Error("Failed to log query", zap.Error(err))
	}
}

// handleSetMode сохраняет выбранный режим для Telegram-пользователя.
// PUT /api/v1/users/mode  body: {"telegram_id": 123, "mode": "simple"}
func (s *Server) handleSetMode(c *gin.Context) {
	var req models.SetModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid request", Code: http.StatusBadRequest, Details: err.Error()})
		return
	}
	if req.Mode != "simple" && req.Mode != "analytics" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "mode must be 'simple' or 'analytics'", Code: http.StatusBadRequest})
		return
	}

	_, err := s.db.Exec(
		`UPDATE users SET preferred_mode = $1 WHERE telegram_id = $2`,
		req.Mode, req.TelegramID,
	)
	if err != nil {
		s.logger.Error("Failed to set user mode", zap.Error(err))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to update mode", Code: http.StatusInternalServerError})
		return
	}
	c.JSON(http.StatusOK, gin.H{"mode": req.Mode})
}

// handleGetMode возвращает сохранённый режим Telegram-пользователя.
// GET /api/v1/users/mode?telegram_id=123
func (s *Server) handleGetMode(c *gin.Context) {
	telegramIDStr := c.Query("telegram_id")
	if telegramIDStr == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "telegram_id is required", Code: http.StatusBadRequest})
		return
	}

	var mode string
	err := s.db.QueryRow(
		`SELECT COALESCE(preferred_mode, 'analytics') FROM users WHERE telegram_id = $1`,
		telegramIDStr,
	).Scan(&mode)

	if err != nil {
		// Пользователь ещё не создан — возвращаем дефолт
		c.JSON(http.StatusOK, gin.H{"mode": "analytics"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"mode": mode})
}

// findOrCreateTelegramUser возвращает user_id для Telegram-пользователя,
// создавая запись в БД при первом обращении.
func (s *Server) findOrCreateTelegramUser(telegramID int64, username string) (string, error) {
	var userID string
	err := s.db.QueryRow(
		`SELECT id FROM users WHERE telegram_id = $1`, telegramID,
	).Scan(&userID)
	if err == nil {
		return userID, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("lookup telegram user: %w", err)
	}

	// Пользователь не найден — создаём
	safeUsername := fmt.Sprintf("tg_%d", telegramID)
	email := fmt.Sprintf("tg_%d@telegram.local", telegramID)
	// Пустой хэш пароля: такой пользователь авторизуется только через telegram_id
	err = s.db.QueryRow(`
		INSERT INTO users (username, email, password_hash, telegram_id)
		VALUES ($1, $2, '', $3)
		ON CONFLICT (telegram_id) DO UPDATE SET username = EXCLUDED.username
		RETURNING id
	`, safeUsername, email, telegramID).Scan(&userID)
	if err != nil {
		return "", fmt.Errorf("create telegram user: %w", err)
	}

	s.logger.Info("Created new Telegram user",
		zap.Int64("telegram_id", telegramID),
		zap.String("username", safeUsername),
		zap.String("user_id", userID),
	)
	return userID, nil
}

// getChatSummary returns compact long-term memory for the user conversation.
func (s *Server) getChatSummary(userID string) (string, error) {
	summary, err := s.getChatSummaryDetails(userID)
	return summary.Summary, err
}

func (s *Server) getChatSummaryDetails(userID string) (models.ChatSummary, error) {
	summary := models.ChatSummary{UserID: userID}
	err := s.db.QueryRow(`
		SELECT user_id, summary, message_count, updated_at
		FROM chat_summaries
		WHERE user_id = $1
	`, userID).Scan(&summary.UserID, &summary.Summary, &summary.MessageCount, &summary.UpdatedAt)
	if err == sql.ErrNoRows {
		return summary, nil
	}
	return summary, err
}

// getChatHistory возвращает последние limit сообщений пользователя (в хронологическом порядке).
func (s *Server) getChatHistory(userID string, limit int) ([]models.ChatMessage, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, role, content, created_at
		FROM (
			SELECT id, user_id, role, content, created_at
			FROM chat_messages
			WHERE user_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		) sub
		ORDER BY created_at ASC
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []models.ChatMessage
	for rows.Next() {
		var m models.ChatMessage
		if err := rows.Scan(&m.ID, &m.UserID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// saveChatMessage сохраняет одно сообщение в истории диалога.
func (s *Server) saveChatMessage(userID, role, content string) {
	_, err := s.db.Exec(`
		INSERT INTO chat_messages (user_id, role, content)
		VALUES ($1, $2, $3)
	`, userID, role, content)
	if err != nil {
		s.logger.Error("Failed to save chat message",
			zap.String("user_id", userID),
			zap.String("role", role),
			zap.Error(err),
		)
	}
}

// updateChatSummary refreshes compact memory after a successful assistant response.
func (s *Server) updateChatSummary(userID, existingSummary, userPrompt, assistantResponse string) error {
	summaryURL := s.simpleProxyURL
	if summaryURL == "" {
		summaryURL = s.modelProxyURL
	}
	if summaryURL == "" {
		return fmt.Errorf("summary orchestrator URL is empty")
	}

	summaryPrompt := buildSummaryUpdatePrompt(existingSummary, userPrompt, assistantResponse)
	updatedSummary, _, err := s.callModelProxy(summaryURL, summaryPrompt)
	if err != nil {
		return err
	}

	updatedSummary = strings.TrimSpace(updatedSummary)
	if updatedSummary == "" {
		return fmt.Errorf("summary orchestrator returned empty summary")
	}

	_, err = s.db.Exec(`
		INSERT INTO chat_summaries (user_id, summary, message_count, updated_at)
		VALUES ($1, $2, 2, CURRENT_TIMESTAMP)
		ON CONFLICT (user_id) DO UPDATE SET
			summary = EXCLUDED.summary,
			message_count = chat_summaries.message_count + 2,
			updated_at = CURRENT_TIMESTAMP
	`, userID, updatedSummary)
	return err
}

func buildSummaryUpdatePrompt(existingSummary, userPrompt, assistantResponse string) string {
	if strings.TrimSpace(existingSummary) == "" {
		existingSummary = "No previous summary."
	}

	var sb strings.Builder
	sb.WriteString("Update the conversation summary for future context.\n")
	sb.WriteString("Keep it concise, factual, and in the same language as the conversation.\n")
	sb.WriteString("Return only the updated summary without headings or markdown.\n\n")
	sb.WriteString("Existing summary:\n")
	sb.WriteString(existingSummary)
	sb.WriteString("\n\nNew exchange:\nUser: ")
	sb.WriteString(userPrompt)
	sb.WriteString("\nAssistant: ")
	sb.WriteString(assistantResponse)
	return sb.String()
}

// buildPromptWithContext оборачивает текущий вопрос резюме и историей предыдущего диалога.
func buildPromptWithContext(summary string, history []models.ChatMessage, prompt string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" && len(history) == 0 {
		return prompt
	}

	var sb strings.Builder
	if summary != "" {
		sb.WriteString("Conversation summary:\n")
		sb.WriteString(summary)
		sb.WriteString("\n\n")
	}
	if len(history) > 0 {
		sb.WriteString("Recent messages:\n")
		for _, msg := range history {
			switch msg.Role {
			case "user":
				sb.WriteString("Пользователь: ")
			case "assistant":
				sb.WriteString("Ассистент: ")
			default:
				sb.WriteString(msg.Role)
				sb.WriteString(": ")
			}
			sb.WriteString(msg.Content)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Current user question: ")
	sb.WriteString(prompt)
	return sb.String()
}

// Генерация ключа для кэша (оставлен для возможного будущего использования)
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
