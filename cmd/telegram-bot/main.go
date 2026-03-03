package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"go.uber.org/zap"

	"github.com/hlebysq/llm-intelligent-system/internal/logger"
)

type TelegramBot struct {
	bot           *tgbotapi.BotAPI
	logger        *zap.Logger
	apiGatewayURL string
	jwtToken      string
	httpClient    *http.Client
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
	log.Info("Starting Telegram Bot", zap.String("environment", env))

	// Инициализация Telegram Bot API
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatal("Failed to create bot", zap.Error(err))
	}

	// Включение debug режима в development
	if env == "development" {
		bot.Debug = true
	}

	log.Info("Authorized on account", zap.String("username", bot.Self.UserName))

	// Создание HTTP клиента
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
	}

	// Создание бота
	telegramBot := &TelegramBot{
		bot:           bot,
		logger:        log,
		apiGatewayURL: getEnv("API_GATEWAY_URL", "http://localhost:8080"),
		httpClient:    httpClient,
	}

	// Получение JWT токена
	if err := telegramBot.authenticate(); err != nil {
		log.Fatal("Failed to authenticate", zap.Error(err))
	}

	// Настройка обновлений
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("Bot is running. Press Ctrl+C to stop.")

	// Обработка обновлений
	go telegramBot.handleUpdates(updates)

	// Ожидание сигнала завершения
	<-quit
	log.Info("Shutting down bot...")
	bot.StopReceivingUpdates()
	log.Info("Bot stopped")
}

// Аутентификация в API Gateway
func (tb *TelegramBot) authenticate() error {
	loginURL := tb.apiGatewayURL + "/api/v1/auth/login"

	requestBody := map[string]string{
		"username": "testuser",
		"password": "password123",
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	resp, err := tb.httpClient.Post(loginURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to call login API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(body))
	}

	var loginResp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("failed to decode login response: %w", err)
	}

	tb.jwtToken = loginResp.Token
	tb.logger.Info("Successfully authenticated with API Gateway")

	return nil
}

// Обработка обновлений от Telegram
func (tb *TelegramBot) handleUpdates(updates tgbotapi.UpdatesChannel) {
	for update := range updates {
		if update.Message == nil {
			continue
		}

		// Логирование входящего сообщения
		tb.logger.Info("Received message",
			zap.Int64("chat_id", update.Message.Chat.ID),
			zap.String("username", update.Message.From.UserName),
			zap.String("text", update.Message.Text),
		)

		// Обработка команд
		if update.Message.IsCommand() {
			tb.handleCommand(update.Message)
			continue
		}

		// Обработка обычных сообщений
		tb.handleMessage(update.Message)
	}
}

// Обработка команд
func (tb *TelegramBot) handleCommand(message *tgbotapi.Message) {
	switch message.Command() {
	case "start":
		tb.sendMessage(message.Chat.ID,
			"👋 Привет! Я бот для работы с языковыми моделями.\n\n"+
				"Просто отправь мне любой вопрос, и я передам его нейросети для обработки!\n\n"+
				"Команды:\n"+
				"/start - начать работу\n"+
				"/help - помощь\n"+
				"/history - история запросов",
		)

	case "help":
		tb.sendMessage(message.Chat.ID,
			"ℹ️ Как пользоваться:\n\n"+
				"1. Просто напиши свой вопрос\n"+
				"2. Я отправлю его языковой модели\n"+
				"3. Получишь ответ через несколько секунд\n\n"+
				"Примеры:\n"+
				"- Объясни квантовую физику простыми словами\n"+
				"- Напиши стихотворение про осень\n"+
				"- Как работает блокчейн?",
		)

	case "history":
		tb.handleHistoryCommand(message.Chat.ID)

	default:
		tb.sendMessage(message.Chat.ID,
			"❓ Неизвестная команда. Используй /help для списка команд.",
		)
	}
}

// Обработка текстовых сообщений
func (tb *TelegramBot) handleMessage(message *tgbotapi.Message) {
	chatID := message.Chat.ID

	// Отправка индикатора "печатает..."
	typingAction := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	tb.bot.Send(typingAction)

	// Отправка запроса к API Gateway
	response, processingTime, err := tb.queryLLM(message.Text)
	if err != nil {
		tb.logger.Error("Failed to query LLM",
			zap.Error(err),
			zap.Int64("chat_id", chatID),
		)
		tb.sendMessage(chatID,
			"❌ Произошла ошибка при обработке запроса. Попробуйте позже.",
		)
		return
	}

	// Формирование ответа с метаданными
	fullResponse := fmt.Sprintf(
		"%s\n\n⏱️ Время обработки: %dмс",
		response,
		processingTime,
	)

	// Отправка ответа
	tb.sendMessage(chatID, fullResponse)
}

// Запрос к LLM через API Gateway
func (tb *TelegramBot) queryLLM(prompt string) (string, int64, error) {
	queryURL := tb.apiGatewayURL + "/api/v1/query"

	requestBody := map[string]string{
		"prompt": prompt,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", queryURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tb.jwtToken)

	resp, err := tb.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to call API: %w", err)
	}
	defer resp.Body.Close()

	// Если токен истек, переавторизуемся
	if resp.StatusCode == http.StatusUnauthorized {
		tb.logger.Warn("Token expired, re-authenticating...")
		if err := tb.authenticate(); err != nil {
			return "", 0, fmt.Errorf("failed to re-authenticate: %w", err)
		}

		// Повторяем запрос с новым токеном
		req.Header.Set("Authorization", "Bearer "+tb.jwtToken)
		resp, err = tb.httpClient.Do(req)
		if err != nil {
			return "", 0, fmt.Errorf("failed to call API after re-auth: %w", err)
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var queryResp struct {
		Response       string `json:"response"`
		ModelUsed      string `json:"model_used"`
		ProcessingTime int64  `json:"processing_time_ms"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&queryResp); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return queryResp.Response, queryResp.ProcessingTime, nil
}

// Обработка команды /history
func (tb *TelegramBot) handleHistoryCommand(chatID int64) {
	historyURL := tb.apiGatewayURL + "/api/v1/history"

	req, err := http.NewRequest("GET", historyURL, nil)
	if err != nil {
		tb.sendMessage(chatID, "❌ Ошибка при получении истории")
		return
	}

	req.Header.Set("Authorization", "Bearer "+tb.jwtToken)

	resp, err := tb.httpClient.Do(req)
	if err != nil {
		tb.sendMessage(chatID, "❌ Ошибка при получении истории")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		tb.sendMessage(chatID, "❌ Не удалось получить историю")
		return
	}

	var historyResp struct {
		History []struct {
			OriginalQuery string    `json:"original_query"`
			ModelUsed     string    `json:"model_used"`
			LatencyMS     int       `json:"latency_ms"`
			CreatedAt     time.Time `json:"created_at"`
		} `json:"history"`
		Count int `json:"count"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&historyResp); err != nil {
		tb.sendMessage(chatID, "❌ Ошибка при обработке ответа")
		return
	}

	if historyResp.Count == 0 {
		tb.sendMessage(chatID, "📭 История запросов пуста")
		return
	}

	// Формирование сообщения с историей
	message := fmt.Sprintf("📚 История запросов (последние %d):\n\n", historyResp.Count)
	for i, item := range historyResp.History {
		if i >= 10 { // Показываем максимум 10 записей
			break
		}

		// Обрезаем длинные запросы
		query := item.OriginalQuery
		if len(query) > 50 {
			query = query[:47] + "..."
		}

		message += fmt.Sprintf(
			"%d. %s\n   🤖 %s | ⏱️ %dмс | 📅 %s\n\n",
			i+1,
			query,
			item.ModelUsed,
			item.LatencyMS,
			item.CreatedAt.Format("02.01 15:04"),
		)
	}
	tb.logger.Debug(message)
	tb.sendMessage(chatID, message)
}

// Отправка сообщения пользователю
func (tb *TelegramBot) sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"

	if _, err := tb.bot.Send(msg); err != nil {
		tb.logger.Error("Failed to send message",
			zap.Error(err),
			zap.Int64("chat_id", chatID),
		)
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
