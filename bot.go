package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

const (
	TopicTasks     = "tasks"
	TopicReminders = "reminders"
	TopicShopping  = "shopping"
	TopicBasket    = "basket"

	StatusActive = "active"
)

type ChatState struct {
	Topic        string
	LastActivity time.Time
}

type Store struct {
	DB *sql.DB
}

type Item struct {
	ID        int64
	ChatID    int64
	Topic     string
	Text      string
	CreatedAt time.Time
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		log.Fatalf("missing required env %s", key)
	}
	return v
}

func envOr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func openStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	ddl := `
CREATE TABLE IF NOT EXISTS items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id INTEGER NOT NULL,
  topic TEXT NOT NULL,
  text TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_chat_topic_status ON items(chat_id, topic, status);

CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`
	_, err := s.DB.Exec(ddl)
	return err
}

func (s *Store) SetKV(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

func (s *Store) GetKV(key string) (string, bool) {
	var v string
	err := s.DB.QueryRow(`SELECT v FROM kv WHERE k=?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) AddItem(chatID int64, topic, text string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(
		`INSERT INTO items(chat_id, topic, text, status, created_at) VALUES(?,?,?,?,?)`,
		chatID, topic, text, StatusActive, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListActive(chatID int64, topic string) ([]Item, error) {
	q := `SELECT id, chat_id, topic, text, created_at FROM items WHERE chat_id=? AND status=?`
	args := []any{chatID, StatusActive}
	if topic != "" {
		q += ` AND topic=?`
		args = append(args, topic)
	}
	q += ` ORDER BY id ASC`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var it Item
		var created string
		if err := rows.Scan(&it.ID, &it.ChatID, &it.Topic, &it.Text, &created); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, created)
		it.CreatedAt = t
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) DeleteItem(chatID, id int64) error {
	_, err := s.DB.Exec(`DELETE FROM items WHERE chat_id=? AND id=?`, chatID, id)
	return err
}

func (s *Store) DeleteAllReminders(chatID int64) error {
	_, err := s.DB.Exec(`DELETE FROM items WHERE chat_id=? AND topic=?`, chatID, TopicReminders)
	return err
}

func topicLabel(topic string) string {
	switch topic {
	case TopicTasks:
		return "ЗАДАЧИ"
	case TopicReminders:
		return "НАПОМИНАНИЯ"
	case TopicShopping:
		return "ПОКУПКИ"
	case TopicBasket:
		return "КОРЗИНУ"
	default:
		return strings.ToUpper(topic)
	}
}

func isTopicButtonText(t string) (string, bool) {
	switch strings.TrimSpace(strings.ToLower(t)) {
	case "задачи":
		return TopicTasks, true
	case "напоминания":
		return TopicReminders, true
	case "покупки":
		return TopicShopping, true
	case "корзина":
		return TopicBasket, true
	case "menu":
		return TopicBasket, true
	default:
		return "", false
	}
}

func mainMenuKeyboard() tgbotapi.ReplyKeyboardMarkup {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Задачи"),
			tgbotapi.NewKeyboardButton("Напоминания"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Покупки"),
			tgbotapi.NewKeyboardButton("Корзина"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("menu"),
		),
	)
	kb.ResizeKeyboard = true
	kb.OneTimeKeyboard = false
	kb.Selective = true
	return kb
}

type App struct {
	Bot        *tgbotapi.BotAPI
	Store      *Store
	Calendar   CalendarClient
	TZ         *time.Location
	TTL        time.Duration
	StateMu    sync.Mutex
	ChatStates map[int64]*ChatState
}

func (a *App) getState(chatID int64) *ChatState {
	a.StateMu.Lock()
	defer a.StateMu.Unlock()

	st := a.ChatStates[chatID]
	if st == nil {
		st = &ChatState{Topic: TopicBasket, LastActivity: time.Now().In(a.TZ)}
		a.ChatStates[chatID] = st
	}
	return st
}

func (a *App) touchState(chatID int64) *ChatState {
	a.StateMu.Lock()
	defer a.StateMu.Unlock()

	now := time.Now().In(a.TZ)
	st := a.ChatStates[chatID]
	if st == nil {
		st = &ChatState{Topic: TopicBasket, LastActivity: now}
		a.ChatStates[chatID] = st
		return st
	}
	// TTL: if inactive too long -> basket
	if now.Sub(st.LastActivity) > a.TTL {
		st.Topic = TopicBasket
	}
	st.LastActivity = now
	return st
}

func (a *App) setTopic(chatID int64, topic string) {
	a.StateMu.Lock()
	defer a.StateMu.Unlock()

	now := time.Now().In(a.TZ)
	st := a.ChatStates[chatID]
	if st == nil {
		st = &ChatState{}
		a.ChatStates[chatID] = st
	}
	st.Topic = topic
	st.LastActivity = now
}

func (a *App) resetToMenu(chatID int64) {
	a.StateMu.Lock()
	defer a.StateMu.Unlock()

	now := time.Now().In(a.TZ)
	st := a.ChatStates[chatID]
	if st == nil {
		st = &ChatState{}
		a.ChatStates[chatID] = st
	}
	st.Topic = TopicBasket
	st.LastActivity = now
}

func (a *App) ensureChatID(chatID int64) {
	// If CHAT_ID env is not set, we persist the first seen chat id into kv so the scheduler can send proactive messages.
	if strings.TrimSpace(os.Getenv("CHAT_ID")) != "" {
		return
	}
	_ = a.Store.SetKV("chat_id", strconv.FormatInt(chatID, 10))
}

func (a *App) send(chatID int64, text string, opts ...func(*tgbotapi.MessageConfig)) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = mainMenuKeyboard()
	for _, o := range opts {
		o(&msg)
	}
	_, _ = a.Bot.Send(msg)
}

func (a *App) run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := a.Bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case upd := <-updates:
			if upd.Message != nil {
				a.handleMessage(ctx, upd.Message)
			}
			if upd.CallbackQuery != nil {
				a.handleCallback(ctx, upd.CallbackQuery)
			}
		}
	}
}

func (a *App) handleMessage(ctx context.Context, m *tgbotapi.Message) {
	chatID := m.Chat.ID
	a.ensureChatID(chatID)

	// Commands
	if m.IsCommand() {
		switch m.Command() {
		case "start":
			a.resetToMenu(chatID)
			a.send(chatID, "Меню открыто. Режим по умолчанию: КОРЗИНА.")
		case "menu":
			a.resetToMenu(chatID)
			a.send(chatID, "Меню открыто. Режим по умолчанию: КОРЗИНА.")
		case "list":
			st := a.getState(chatID)
			items, err := a.Store.ListActive(chatID, st.Topic)
			if err != nil {
				a.send(chatID, "Ошибка чтения списка.")
				return
			}
			if st.Topic == TopicReminders {
				a.send(chatID, "НАПОМИНАНИЯ:")
				a.sendRemindersOneByOne(chatID, items)
				return
			}
			a.send(chatID, formatItems(st.Topic, items))
		default:
			a.send(chatID, "Неизвестная команда. Используй /start или /menu.")
		}
		return
	}

	// Only text messages supported in this version
	if m.Text == "" {
		a.send(chatID, "Понимаю только текстовые сообщения.")
		return
	}

	// Menu/topic buttons
	if topic, ok := isTopicButtonText(m.Text); ok {
		btn := strings.ToLower(strings.TrimSpace(m.Text))

		// "menu" forces basket
		if btn == "menu" {
			a.resetToMenu(chatID)
			items, err := a.Store.ListActive(chatID, TopicBasket)
			if err != nil {
				a.send(chatID, "Меню открыто. Режим: КОРЗИНА. Ошибка чтения корзины.")
				return
			}
			a.send(chatID, "Меню открыто. Режим: КОРЗИНА.")
			// show current basket snapshot
			a.send(chatID, formatItems(TopicBasket, items))
			return
		}

		a.setTopic(chatID, topic)

		// Immediately show current list for selected topic
		items, err := a.Store.ListActive(chatID, topic)
		if err != nil {
			a.send(chatID, fmt.Sprintf("Режим: %s. Ошибка чтения списка.", topicLabel(topic)))
			return
		}

		if topic == TopicReminders {
			a.send(chatID, fmt.Sprintf("Режим: %s.", topicLabel(topic)))
			a.sendRemindersOneByOne(chatID, items)
			return
		}

		a.send(chatID, fmt.Sprintf("Режим: %s.", topicLabel(topic)))
		a.send(chatID, formatItems(topic, items))
		return
	}

	// Normal text -> add to current topic (with TTL check)
	st := a.touchState(chatID)
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	if _, err := a.Store.AddItem(chatID, st.Topic, text); err != nil {
		a.send(chatID, "Ошибка записи в БД.")
		return
	}
	a.send(chatID, fmt.Sprintf("ДОБАВИЛ СООБЩЕНИЕ В %s.", topicLabel(st.Topic)))
}

func (a *App) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	a.ensureChatID(chatID)
	data := strings.TrimSpace(cq.Data)

	// callback format: done:<id>
	if strings.HasPrefix(data, "done:") {
		idStr := strings.TrimPrefix(data, "done:")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err == nil {
			_ = a.Store.DeleteItem(chatID, id)
		}
		// Acknowledge callback
		_, _ = a.Bot.Request(tgbotapi.NewCallback(cq.ID, "Удалено"))

		// Edit the original message to show completion and remove buttons
		edit := tgbotapi.NewEditMessageText(chatID, cq.Message.MessageID, "✅ Удалено")
		_, _ = a.Bot.Send(edit)
		editMarkup := tgbotapi.NewEditMessageReplyMarkup(chatID, cq.Message.MessageID, tgbotapi.InlineKeyboardMarkup{})
		_, _ = a.Bot.Send(editMarkup)
		return
	}

	_, _ = a.Bot.Request(tgbotapi.NewCallback(cq.ID, ""))
}

func (a *App) sendRemindersOneByOne(chatID int64, items []Item) {
	if len(items) == 0 {
		a.send(chatID, "Пусто.")
		return
	}
	for _, it := range items {
		msg := tgbotapi.NewMessage(chatID, formatSingleReminder(it))
		msg.ReplyMarkup = singleReminderKeyboard(it.ID)
		_, _ = a.Bot.Send(msg)
	}
}

func singleReminderKeyboard(id int64) tgbotapi.InlineKeyboardMarkup {
	btn := tgbotapi.NewInlineKeyboardButtonData("✅", fmt.Sprintf("done:%d", id))
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(btn))
}

func formatSingleReminder(it Item) string {
	return fmt.Sprintf("НАПОМИНАНИЕ #%d: %s", it.ID, it.Text)
}

func formatItems(topic string, items []Item) string {
	if len(items) == 0 {
		return fmt.Sprintf("%s: пусто.", topicLabel(topic))
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s:\n", topicLabel(topic)))
	for i, it := range items {
		b.WriteString(fmt.Sprintf("%d) %s\n", i+1, it.Text))
	}
	return strings.TrimSpace(b.String())
}

func formatReminders(items []Item) string {
	if len(items) == 0 {
		return "НАПОМИНАНИЯ: пусто."
	}
	var b strings.Builder
	b.WriteString("НАПОМИНАНИЯ:\n")
	for i, it := range items {
		b.WriteString(fmt.Sprintf("%d) %s\n", i+1, it.Text))
	}
	b.WriteString("\n(Нажми ✅ чтобы удалить пункт)")
	return strings.TrimSpace(b.String())
}

func initApp() (*App, error) {
	// Timezone
	tzName := envOr("TZ", "Europe/Moscow")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("bad TZ %q: %w", tzName, err)
	}

	// Store
	dbPath := envOr("DB_PATH", "gtd.db")
	store, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}

	// Telegram bot
	token := mustEnv("BOT_TOKEN")
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	bot.Debug = strings.EqualFold(envOr("BOT_DEBUG", "false"), "true")

	ttlMin, _ := strconv.Atoi(envOr("TTL_MINUTES", "10"))
	ttl := time.Duration(ttlMin) * time.Minute

	// Calendar client (stub unless configured)
	cal, _ := NewGoogleCalendarClientFromEnv(loc)

	return &App{
		Bot:        bot,
		Store:      store,
		Calendar:   cal,
		TZ:         loc,
		TTL:        ttl,
		ChatStates: map[int64]*ChatState{},
	}, nil
}

func main() {
	godotenv.Load()
	app, err := initApp()
	if err != nil {
		log.Fatal(err)
	}
	defer app.Store.DB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Scheduler
	s := NewScheduler(app.Bot, app.Store, app.Calendar, app.TZ)
	s.Start(ctx)

	log.Printf("bot started as @%s", app.Bot.Self.UserName)
	if err := app.run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
