package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"go.uber.org/zap"

	"github.com/hlebysq/llm-intelligent-system/internal/logger"
)

const (
	modeSimple    = "simple"
	modeAnalytics = "analytics"
)

type queryLimitError struct {
	mode string
}

func (e *queryLimitError) Error() string {
	return "daily query limit exceeded for mode " + e.mode
}

type TelegramBot struct {
	bot           *tgbotapi.BotAPI
	logger        *zap.Logger
	apiGatewayURL string
	jwtToken      string
	httpClient    *http.Client

	modesMu sync.RWMutex
	modes   map[int64]string // chatID → режим
}

// getMode возвращает текущий режим для чата (только из in-memory кэша).
func (tb *TelegramBot) getMode(chatID int64) string {
	tb.modesMu.RLock()
	defer tb.modesMu.RUnlock()
	if m, ok := tb.modes[chatID]; ok {
		return m
	}
	return modeAnalytics
}

// setMode сохраняет режим в памяти.
func (tb *TelegramBot) setMode(chatID int64, mode string) {
	tb.modesMu.Lock()
	defer tb.modesMu.Unlock()
	tb.modes[chatID] = mode
}

// ensureModeLoaded подгружает режим из БД при первом обращении после рестарта бота.
func (tb *TelegramBot) ensureModeLoaded(chatID int64) {
	tb.modesMu.RLock()
	_, loaded := tb.modes[chatID]
	tb.modesMu.RUnlock()
	if loaded {
		return
	}

	mode, err := tb.fetchUserMode(chatID)
	if err != nil {
		tb.logger.Warn("Failed to fetch user mode, using default",
			zap.Int64("chat_id", chatID), zap.Error(err))
		mode = modeAnalytics
	}
	tb.setMode(chatID, mode)
}

// fetchUserMode запрашивает сохранённый режим пользователя через API Gateway.
func (tb *TelegramBot) fetchUserMode(chatID int64) (string, error) {
	url := fmt.Sprintf("%s/api/v1/users/mode?telegram_id=%d", tb.apiGatewayURL, chatID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return modeAnalytics, err
	}
	req.Header.Set("Authorization", "Bearer "+tb.jwtToken)

	resp, err := tb.httpClient.Do(req)
	if err != nil {
		return modeAnalytics, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return modeAnalytics, fmt.Errorf("gateway returned %d", resp.StatusCode)
	}

	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return modeAnalytics, err
	}
	return body.Mode, nil
}

// persistUserMode сохраняет режим пользователя в БД через API Gateway.
func (tb *TelegramBot) persistUserMode(chatID int64, mode string) {
	url := tb.apiGatewayURL + "/api/v1/users/mode"
	body, _ := json.Marshal(map[string]interface{}{
		"telegram_id": chatID,
		"mode":        mode,
	})

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(body))
	if err != nil {
		tb.logger.Error("Failed to create mode request", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tb.jwtToken)

	resp, err := tb.httpClient.Do(req)
	if err != nil {
		tb.logger.Error("Failed to persist user mode", zap.Error(err))
		return
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		tb.logger.Warn("Unexpected status when persisting mode",
			zap.Int("status", resp.StatusCode), zap.Int64("chat_id", chatID))
	}
}

func main() {
	_ = godotenv.Load()

	env := getEnv("ENVIRONMENT", "development")
	if err := logger.InitLogger(env); err != nil {
		panic(fmt.Sprintf("Failed to initialize logger: %v", err))
	}
	defer logger.Sync()

	log := logger.GetLogger()
	log.Info("Starting Telegram Bot", zap.String("environment", env))

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatal("Failed to create bot", zap.Error(err))
	}

	if env == "development" {
		bot.Debug = true
	}

	log.Info("Authorized on account", zap.String("username", bot.Self.UserName))

	httpClient := &http.Client{
		Timeout: 600 * time.Second,
	}

	telegramBot := &TelegramBot{
		bot:           bot,
		logger:        log,
		apiGatewayURL: getEnv("API_GATEWAY_URL", "http://localhost:8080"),
		httpClient:    httpClient,
		modes:         make(map[int64]string),
	}

	if err := telegramBot.authenticate(); err != nil {
		log.Fatal("Failed to authenticate", zap.Error(err))
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("Bot is running. Press Ctrl+C to stop.")

	go telegramBot.handleUpdates(updates)

	<-quit
	log.Info("Shutting down bot...")
	bot.StopReceivingUpdates()
	log.Info("Bot stopped")
}

// Аутентификация в API Gateway (сервисный аккаунт Telegram-бота)
func (tb *TelegramBot) authenticate() error {
	loginURL := tb.apiGatewayURL + "/api/v1/auth/login"

	requestBody := map[string]string{
		"username": "telegram_bot",
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
		// Нажатие inline-кнопки
		if update.CallbackQuery != nil {
			tb.handleCallbackQuery(update.CallbackQuery)
			continue
		}

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

// handleCallbackQuery обрабатывает нажатия inline-кнопок (смена режима).
func (tb *TelegramBot) handleCallbackQuery(query *tgbotapi.CallbackQuery) {
	// Подтверждаем callback, чтобы убрать индикатор загрузки на кнопке
	tb.bot.Request(tgbotapi.NewCallback(query.ID, ""))

	chatID := query.Message.Chat.ID
	msgID := query.Message.MessageID

	switch query.Data {
	case "mode:simple":
		tb.setMode(chatID, modeSimple)
		go tb.persistUserMode(chatID, modeSimple)
		tb.editMessage(chatID, msgID,
			"✅ Режим переключён: 💬 Обычный\n\n"+
				"Один быстрый вызов модели — идеально для коротких вопросов.")
	case "mode:analytics":
		tb.setMode(chatID, modeAnalytics)
		go tb.persistUserMode(chatID, modeAnalytics)
		tb.editMessage(chatID, msgID,
			"✅ Режим переключён: 🔬 Аналитический\n\n"+
				"Консилиум из нескольких моделей — для сложных и неоднозначных вопросов.")
	}
}

// Обработка команд
func (tb *TelegramBot) handleCommand(message *tgbotapi.Message) {
	switch message.Command() {
	case "start":
		tb.sendMessage(message.Chat.ID,
			"👋 Привет! Я бот для работы с языковыми моделями.\n\n"+
				"Просто отправь мне любой вопрос!\n\n"+
				"Команды:\n"+
				"/start - начать работу\n"+
				"/help - помощь\n"+
				"/mode - сменить режим обработки\n"+
				"/history - контекст диалога",
		)

	case "help":
		tb.sendMessage(message.Chat.ID,
			"ℹ️ Как пользоваться:\n\n"+
				"1. Напиши свой вопрос\n"+
				"2. Выбери режим командой /mode:\n"+
				"   💬 Обычный — быстрый ответ от одной модели\n"+
				"   🔬 Аналитический — консилиум моделей, дольше но точнее\n"+
				"3. Получишь ответ\n\n"+
				"Примеры:\n"+
				"- Объясни квантовую физику простыми словами\n"+
				"- Напиши стихотворение про осень\n"+
				"- Как работает блокчейн?",
		)

	case "mode":
		tb.handleModeCommand(message.Chat.ID)

	case "history":
		tb.handleHistoryCommandV2(message.Chat.ID)

	default:
		tb.sendMessage(message.Chat.ID,
			"❓ Неизвестная команда. Используй /help для списка команд.",
		)
	}
}

// handleModeCommand показывает текущий режим и кнопки для переключения.
func (tb *TelegramBot) handleModeCommand(chatID int64) {
	tb.ensureModeLoaded(chatID)
	current := tb.getMode(chatID)

	var currentLabel string
	if current == modeSimple {
		currentLabel = "💬 Обычный"
	} else {
		currentLabel = "🔬 Аналитический"
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💬 Обычный", "mode:simple"),
			tgbotapi.NewInlineKeyboardButtonData("🔬 Аналитический", "mode:analytics"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
		"⚙️ Текущий режим: %s\n\nВыбери режим обработки запросов:", currentLabel,
	))
	msg.ReplyMarkup = keyboard

	if _, err := tb.bot.Send(msg); err != nil {
		tb.logger.Error("Failed to send mode keyboard", zap.Error(err))
	}
}

// Обработка текстовых сообщений
func (tb *TelegramBot) handleMessage(message *tgbotapi.Message) {
	chatID := message.Chat.ID
	tb.ensureModeLoaded(chatID)
	mode := tb.getMode(chatID)

	type progressStage struct {
		delay time.Duration
		text  string
	}

	var stages []progressStage
	var initialText string

	if mode == modeSimple {
		initialText = "⏳ Отправляю запрос к модели..."
		stages = []progressStage{
			{6 * time.Second, "🤔 Модель формулирует ответ..."},
		}
	} else {
		initialText = "⏳ Запрос принят, запускаю консилиум моделей..."
		stages = []progressStage{
			{8 * time.Second, "🤔 Дебатёры формулируют начальные ответы..."},
			{14 * time.Second, "⚔️ Модели анализируют позиции друг друга..."},
			{16 * time.Second, "✍️ Каждый дебатёр уточняет свою версию..."},
			{17 * time.Second, "⚖️ Судья анализирует все позиции и выносит вердикт..."},
		}
	}

	// Сразу отправляем сообщение-заглушку и запоминаем его ID
	statusMsg, err := tb.sendMessageAndGetID(chatID, initialText)
	if err != nil {
		tb.logger.Error("Failed to send initial message", zap.Error(err))
		return
	}
	msgID := statusMsg.MessageID

	// Запускаем LLM-запрос в фоне
	type queryResult struct {
		response string
		procTime int64
		err      error
	}
	resultCh := make(chan queryResult, 1)
	go func() {
		resp, t, err := tb.queryLLM(message.Text, message.Chat.ID, message.From.UserName, mode)
		resultCh <- queryResult{resp, t, err}
	}()

	// Горутина обновляет статусное сообщение по таймерам
	doneCh := make(chan struct{})
	go func() {
		for _, s := range stages {
			select {
			case <-time.After(s.delay):
				tb.editMessage(chatID, msgID, s.text)
			case <-doneCh:
				return
			}
		}
	}()

	// Ждём результата и закрываем прогресс
	res := <-resultCh
	close(doneCh)

	if res.err != nil {
		tb.logger.Error("Failed to query LLM",
			zap.Error(res.err),
			zap.Int64("chat_id", chatID),
		)
		var limitErr *queryLimitError
		if errors.As(res.err, &limitErr) {
			tb.editMessage(chatID, msgID, dailyLimitMessage(limitErr.mode))
			return
		}
		tb.editMessage(chatID, msgID, "❌ Произошла ошибка при обработке запроса. Попробуйте позже.")
		return
	}

	finalText := fmt.Sprintf("%s\n\n⏱️ Время обработки: %dмс", res.response, res.procTime)
	tb.editMessage(chatID, msgID, finalText)
}

// Запрос к LLM через API Gateway.
// telegramID и telegramUsername передаются для разделения истории по пользователям.
// mode: "simple" — один вызов модели, "analytics" — полный дебат.
func (tb *TelegramBot) queryLLM(prompt string, telegramID int64, telegramUsername string, mode string) (string, int64, error) {
	queryURL := tb.apiGatewayURL + "/api/v1/query"

	requestBody := map[string]interface{}{
		"prompt":            prompt,
		"telegram_id":       telegramID,
		"telegram_username": telegramUsername,
		"mode":              mode,
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
		if resp.StatusCode == http.StatusTooManyRequests {
			limitMode := resp.Header.Get("X-RateLimit-Mode")
			if limitMode == "" {
				limitMode = mode
			}
			return "", 0, &queryLimitError{mode: limitMode}
		}
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

func dailyLimitMessage(mode string) string {
	switch mode {
	case modeSimple:
		return "Лимит обычных запросов на сегодня исчерпан. Попробуйте снова завтра или переключитесь на аналитический режим, если лимит там еще доступен."
	default:
		return "Лимит аналитических запросов на сегодня исчерпан. Попробуйте снова завтра или переключитесь на обычный режим."
	}
}

// Обработка команды /history
func (tb *TelegramBot) handleHistoryCommandV2(chatID int64) {
	historyURL := fmt.Sprintf("%s/api/v1/history?telegram_id=%d", tb.apiGatewayURL, chatID)

	req, err := http.NewRequest(http.MethodGet, historyURL, nil)
	if err != nil {
		tb.sendMessage(chatID, "Failed to create history request")
		return
	}
	req.Header.Set("Authorization", "Bearer "+tb.jwtToken)

	resp, err := tb.httpClient.Do(req)
	if err != nil {
		tb.sendMessage(chatID, "Failed to load history")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		tb.sendMessage(chatID, "Could not load history")
		return
	}

	var historyResp struct {
		Summary struct {
			Content      string     `json:"content"`
			MessageCount int        `json:"message_count"`
			UpdatedAt    *time.Time `json:"updated_at"`
		} `json:"summary"`
		Messages []struct {
			Role      string    `json:"role"`
			Content   string    `json:"content"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"messages"`
		Count int `json:"count"`
		Limit int `json:"limit"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&historyResp); err != nil {
		tb.sendMessage(chatID, "Failed to parse history response")
		return
	}

	messages := make([]historyMessageView, 0, len(historyResp.Messages))
	for _, item := range historyResp.Messages {
		messages = append(messages, historyMessageView{
			Role:      item.Role,
			Content:   item.Content,
			CreatedAt: item.CreatedAt,
		})
	}
	message := buildHistoryMessage(historySummaryView{
		Content:      historyResp.Summary.Content,
		MessageCount: historyResp.Summary.MessageCount,
		UpdatedAt:    historyResp.Summary.UpdatedAt,
	}, messages, historyResp.Limit)
	if message == "" {
		tb.sendMessage(chatID, "History is empty")
		return
	}

	tb.logger.Debug(message)
	tb.sendPlainMessage(chatID, message)
}

func buildHistoryMessage(summary historySummaryView, messages []historyMessageView, limit int) string {
	userMessages := make([]historyMessageView, 0, len(messages))
	for _, item := range messages {
		if item.Role == "user" {
			userMessages = append(userMessages, item)
		}
	}

	if len(userMessages) == 0 && summary.Content == "" {
		return ""
	}

	message := "Память диалога\n\n"
	message += "Это контекст, который я храню по нашему разговору. Он помогает мне лучше понимать уточняющие вопросы и не терять нить беседы.\n\n"
	message += "Summary - это короткое сжатое резюме ранней части диалога. Ниже я показываю только ваши последние сообщения; ответы ассистента тут скрыты.\n\n"
	if summary.Content != "" {
		message += "Summary:\n" + summary.Content + "\n\n"
	} else {
		message += "Summary пока не создан. Оно появится, когда в диалоге накопится достаточно контекста.\n\n"
	}
	if len(userMessages) > 0 {
		userLimit := limit / 2
		if userLimit < len(userMessages) {
			userLimit = len(userMessages)
		}
		message += fmt.Sprintf("Ваши последние сохраненные сообщения (%d из %d):\n\n", len(userMessages), userLimit)
		for i, item := range userMessages {
			content := truncateRunes(item.Content, 120)
			message += fmt.Sprintf("%d. %s\n   %s\n\n", i+1, content, item.CreatedAt.Format("02.01 15:04"))
		}
	} else {
		message += "Ваших сообщений в последнем окне контекста пока нет.\n"
	}
	return message
}

type historySummaryView struct {
	Content      string
	MessageCount int
	UpdatedAt    *time.Time
}

type historyMessageView struct {
	Role      string
	Content   string
	CreatedAt time.Time
}

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
		Summary struct {
			Content      string     `json:"content"`
			MessageCount int        `json:"message_count"`
			UpdatedAt    *time.Time `json:"updated_at"`
		} `json:"summary"`
		Messages []struct {
			Role      string    `json:"role"`
			Content   string    `json:"content"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"messages"`
		History []struct {
			OriginalQuery string    `json:"original_query"`
			ModelUsed     string    `json:"model_used"`
			LatencyMS     int       `json:"latency_ms"`
			CreatedAt     time.Time `json:"created_at"`
		} `json:"history"`
		Count int `json:"count"`
		Limit int `json:"limit"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&historyResp); err != nil {
		tb.sendMessage(chatID, "❌ Ошибка при обработке ответа")
		return
	}

	if historyResp.Count == 0 && historyResp.Summary.Content == "" {
		tb.sendMessage(chatID, "рџ“­ РСЃС‚РѕСЂРёСЏ Р·Р°РїСЂРѕСЃРѕРІ РїСѓСЃС‚Р°")
		return
	}

	historyMessage := "рџ“љ РљРѕРЅС‚РµРєСЃС‚ РґРёР°Р»РѕРіР°\n\n"
	if historyResp.Summary.Content != "" {
		historyMessage += "Summary:\n" + historyResp.Summary.Content + "\n\n"
	}
	if historyResp.Count > 0 {
		historyMessage += fmt.Sprintf("РџРѕСЃР»РµРґРЅРёРµ СЃРѕРѕР±С‰РµРЅРёСЏ (%d/%d):\n\n", historyResp.Count, historyResp.Limit)
		for i, item := range historyResp.Messages {
			content := item.Content
			if len(content) > 120 {
				content = content[:117] + "..."
			}
			role := "User"
			if item.Role == "assistant" {
				role = "Assistant"
			}
			historyMessage += fmt.Sprintf("%d. %s: %s\n   %s\n\n", i+1, role, content, item.CreatedAt.Format("02.01 15:04"))
		}
	}
	tb.logger.Debug(historyMessage)
	tb.sendMessage(chatID, historyMessage)
	return

	if false && historyResp.Count == 0 {
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
	text = sanitizeTelegramText(text)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"

	if _, err := tb.bot.Send(msg); err != nil {
		tb.logger.Error("Failed to send message",
			zap.Error(err),
			zap.Int64("chat_id", chatID),
		)
	}
}

func (tb *TelegramBot) sendPlainMessage(chatID int64, text string) {
	text = sanitizeTelegramText(text)
	msg := tgbotapi.NewMessage(chatID, text)

	if _, err := tb.bot.Send(msg); err != nil {
		tb.logger.Error("Failed to send message",
			zap.Error(err),
			zap.Int64("chat_id", chatID),
		)
	}
}

// Отправка сообщения с возвратом объекта (нужен MessageID для последующих правок)
func (tb *TelegramBot) sendMessageAndGetID(chatID int64, text string) (tgbotapi.Message, error) {
	text = sanitizeTelegramText(text)
	msg := tgbotapi.NewMessage(chatID, text)
	sent, err := tb.bot.Send(msg)
	if err != nil {
		tb.logger.Error("Failed to send message", zap.Error(err), zap.Int64("chat_id", chatID))
	}
	return sent, err
}

// Редактирование уже отправленного сообщения
func (tb *TelegramBot) editMessage(chatID int64, messageID int, text string) {
	text = sanitizeTelegramText(text)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if _, err := tb.bot.Send(edit); err != nil {
		tb.logger.Warn("Failed to edit message",
			zap.Error(err),
			zap.Int64("chat_id", chatID),
			zap.Int("message_id", messageID),
		)
	}
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(strings.ToValidUTF8(s, ""))
	if len(runes) <= max {
		return string(runes)
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func sanitizeTelegramText(text string) string {
	return strings.ToValidUTF8(text, "")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
