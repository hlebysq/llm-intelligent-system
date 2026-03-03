package main

import (
	"bytes"
	"context"
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

	"github.com/hlebysq/llm-intelligent-system/internal/logger"
	"github.com/hlebysq/llm-intelligent-system/internal/models"
)

type ModelProxy struct {
	router     *gin.Engine
	logger     *zap.Logger
	ollamaURL  string
	modelName  string
	httpClient *http.Client
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
	log.Info("Starting Model Proxy Service", zap.String("environment", env))

	// Создание HTTP клиента с таймаутами
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Создание прокси сервера
	proxy := &ModelProxy{
		router:     gin.Default(),
		logger:     log,
		ollamaURL:  getEnv("OLLAMA_URL", "http://localhost:11434"),
		modelName:  getEnv("MODEL_NAME", "llama3.2"),
		httpClient: httpClient,
	}

	// Настройка маршрутов
	proxy.setupRoutes()

	// Запуск HTTP сервера
	port := getEnv("MODEL_PROXY_PORT", "8081")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      proxy.router,
		ReadTimeout:  70 * time.Second,
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		log.Info("Model Proxy started",
			zap.String("port", port),
			zap.String("ollama_url", proxy.ollamaURL),
			zap.String("model", proxy.modelName),
		)
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

func (p *ModelProxy) setupRoutes() {
	p.router.Use(gin.Recovery())

	api := p.router.Group("/api/v1")
	{
		api.POST("/generate", p.handleGenerate)
		api.GET("/health", p.handleHealth)
		api.GET("/models", p.handleListModels)
	}
}

// Handler для генерации ответа от LLM
func (p *ModelProxy) handleGenerate(c *gin.Context) {
	var req struct {
		Prompt    string `json:"prompt" binding:"required"`
		MaxTokens int    `json:"max_tokens,omitempty"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error:   "Invalid request body",
			Code:    http.StatusBadRequest,
			Details: err.Error(),
		})
		return
	}

	p.logger.Info("Generating response",
		zap.String("model", p.modelName),
		zap.Int("prompt_length", len(req.Prompt)),
	)

	startTime := time.Now()

	// Вызов Ollama API
	response, err := p.callOllama(req.Prompt)
	if err != nil {
		p.logger.Error("Failed to call Ollama", zap.Error(err))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to generate response",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}

	processingTime := time.Since(startTime).Milliseconds()

	p.logger.Info("Response generated successfully",
		zap.String("model", p.modelName),
		zap.Int64("processing_time_ms", processingTime),
		zap.Int("response_length", len(response)),
	)

	c.JSON(http.StatusOK, gin.H{
		"response":        response,
		"model":           p.modelName,
		"processing_time": processingTime,
		"prompt_tokens":   estimateTokens(req.Prompt),
		"response_tokens": estimateTokens(response),
	})
}

// Handler для health check
func (p *ModelProxy) handleHealth(c *gin.Context) {
	checks := make(map[string]string)

	// Проверка доступности Ollama
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", p.ollamaURL+"/api/tags", nil)
	if err != nil {
		checks["ollama"] = "unhealthy: failed to create request"
	} else {
		resp, err := p.httpClient.Do(req)
		if err != nil {
			checks["ollama"] = "unhealthy: " + err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				checks["ollama"] = "healthy"
			} else {
				checks["ollama"] = fmt.Sprintf("unhealthy: status %d", resp.StatusCode)
			}
		}
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
		Service:   "model-proxy",
		Timestamp: time.Now(),
		Checks:    checks,
	})
}

// Handler для получения списка доступных моделей
func (p *ModelProxy) handleListModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", p.ollamaURL+"/api/tags", nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to fetch models",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to fetch models",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to read response",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{
			Error:   "Failed to parse response",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, result)
}

// Вызов Ollama API для генерации ответа
func (p *ModelProxy) callOllama(prompt string) (string, error) {
	requestBody := models.OllamaGenerateRequest{
		Model:  p.modelName,
		Prompt: prompt,
		Stream: false,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		p.ollamaURL+"/api/generate",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp models.OllamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if ollamaResp.Response == "" {
		return "", fmt.Errorf("empty response from model")
	}

	return ollamaResp.Response, nil
}

// Примерная оценка количества токенов (грубая)
func estimateTokens(text string) int {
	// Простая оценка: ~4 символа = 1 токен
	return len(text) / 4
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
