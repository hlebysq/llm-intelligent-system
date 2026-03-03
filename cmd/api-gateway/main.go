package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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
	router     *gin.Engine
	db         *database.DB
	cache      *cache.RedisClient
	jwtManager *auth.JWTManager
	logger     *zap.Logger
}

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

	// Создание сервера
	server := &Server{
		router:     gin.Default(),
		db:         db,
		cache:      redisClient,
		jwtManager: jwtManager,
		logger:     log,
	}

	// Настройка маршрутов
	server.setupRoutes()

	// Запуск HTTP сервера
	port := getEnv("API_GATEWAY_PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      server.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
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
	}
}

// Middleware для CORS
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

	// Поиск пользователя в БД
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
	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")

	var req models.QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error:   "Invalid request body",
			Code:    http.StatusBadRequest,
			Details: err.Error(),
		})
		return
	}

	s.logger.Info("Processing query",
		zap.String("user_id", userID.(string)),
		zap.String("username", username.(string)),
		zap.String("prompt", req.Prompt[:min(50, len(req.Prompt))]),
	)

	startTime := time.Now()

	// Проверка кэша
	cacheKey := generateCacheKey(req.Prompt)
	ctx := c.Request.Context()

	cachedResponse, err := s.cache.Get(ctx, cacheKey)
	if err == nil {
		s.logger.Info("Cache hit", zap.String("cache_key", cacheKey))
		processingTime := time.Since(startTime).Milliseconds()

		c.JSON(http.StatusOK, models.QueryResponse{
			Response:       cachedResponse,
			ModelUsed:      "cache",
			ProcessingTime: processingTime,
		})
		return
	}

	// Отправка запроса к Model Proxy
	modelProxyURL := "http://model-proxy:8081/api/v1/generate"

	response, err := s.callModelProxy(modelProxyURL, req.Prompt)
	if err != nil {
		s.logger.Error("Failed to call model proxy", zap.Error(err))

		// Логирование ошибки
		s.logQuery(userID.(string), req.Prompt, "", "", 0, "error", err.Error())

		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to process query",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}

	processingTime := time.Since(startTime).Milliseconds()

	// Сохранение в кэш
	_ = s.cache.Set(ctx, cacheKey, response, 1*time.Hour)

	// Логирование запроса
	s.logQuery(userID.(string), req.Prompt, "llama3.2", response,
		int(processingTime), "success", "")

	c.JSON(http.StatusOK, models.QueryResponse{
		Response:       response,
		ModelUsed:      "llama3.2",
		ProcessingTime: processingTime,
	})
}

// Handler для получения истории запросов
func (s *Server) handleHistory(c *gin.Context) {
	userID, _ := c.Get("user_id")

	query := `
		SELECT id, user_id, original_query, model_used, response_text, 
		       latency_ms, status, error_message, created_at
		FROM query_logs
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 50
	`

	rows, err := s.db.Query(query, userID.(string))
	if err != nil {
		s.logger.Error("Failed to fetch history", zap.Error(err))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error: "Failed to fetch history",
			Code:  http.StatusInternalServerError,
		})
		return
	}
	defer rows.Close()

	var logs []models.QueryLog
	for rows.Next() {
		var log models.QueryLog
		if err := rows.Scan(
			&log.ID, &log.UserID, &log.OriginalQuery, &log.ModelUsed,
			&log.ResponseText, &log.LatencyMS, &log.Status,
			&log.ErrorMessage, &log.CreatedAt,
		); err != nil {
			s.logger.Error("Failed to scan row", zap.Error(err))
			continue
		}
		logs = append(logs, log)
	}

	c.JSON(http.StatusOK, gin.H{
		"history": logs,
		"count":   len(logs),
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
	if err := s.cache.HealthCheck(); err != nil {
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

// Вспомогательная функция для вызова Model Proxy
func (s *Server) callModelProxy(url, prompt string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	requestBody := map[string]interface{}{
		"prompt": prompt,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}

	resp, err := client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("model proxy returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	response, ok := result["response"].(string)
	if !ok {
		return "", fmt.Errorf("invalid response format")
	}

	return response, nil
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

// Генерация ключа для кэша
func generateCacheKey(prompt string) string {
	hash := sha256.Sum256([]byte(prompt))
	return "query:" + hex.EncodeToString(hash[:])
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
