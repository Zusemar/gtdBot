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
	return kb
}

type App struct {
	Bot        *tgbotapi.BotAPI
	Store      *Store
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
	a.setTopic(chatID, TopicBasket)
}

func (a *App) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = mainMenuKeyboard()
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

	if m.IsCommand() {
		if m.Command() == "start" || m.Command() == "menu" {
			a.resetToMenu(chatID)
			a.send(chatID, "Меню открыто. Режим по умолчанию: КОРЗИНА.")
		}
		return
	}

	if m.Text == "" {
		a.send(chatID, "Понимаю только текст.")
		return
	}

	if topic, ok := isTopicButtonText(m.Text); ok {
		if strings.ToLower(m.Text) == "menu" {
			a.resetToMenu(chatID)
			items, _ := a.Store.ListActive(chatID, TopicBasket)
			a.send(chatID, "Меню открыто. Режим: КОРЗИНА.")
			a.sendItemsOneByOne(chatID, TopicBasket, items)
			return
		}

		a.setTopic(chatID, topic)
		items, _ := a.Store.ListActive(chatID, topic)

		a.send(chatID, fmt.Sprintf("Режим: %s.", topicLabel(topic)))
		a.sendItemsOneByOne(chatID, topic, items)
		return
	}

	// Normal text -> add to current topic (with TTL check)
	st := a.touchState(chatID)
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}

	_, err := a.Store.AddItem(chatID, st.Topic, text)
	if err != nil {
		a.send(chatID, "Ошибка записи.")
		return
	}
	a.send(chatID, fmt.Sprintf("ДОБАВИЛ СООБЩЕНИЕ В %s.", topicLabel(st.Topic)))
}

func (a *App) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	data := strings.TrimSpace(cq.Data)

	if strings.HasPrefix(data, "done:") {
		idStr := strings.TrimPrefix(data, "done:")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		_ = a.Store.DeleteItem(chatID, id)

		_, _ = a.Bot.Request(tgbotapi.NewCallback(cq.ID, "Удалено"))
		edit := tgbotapi.NewEditMessageText(chatID, cq.Message.MessageID, "✅ Удалено")
		_, _ = a.Bot.Send(edit)
		editMarkup := tgbotapi.NewEditMessageReplyMarkup(chatID, cq.Message.MessageID, tgbotapi.InlineKeyboardMarkup{})
		_, _ = a.Bot.Send(editMarkup)
	}
}

func (a *App) sendItemsOneByOne(chatID int64, topic string, items []Item) {
	if len(items) == 0 {
		a.send(chatID, "Пусто.")
		return
	}
	for _, it := range items {
		msg := tgbotapi.NewMessage(chatID, formatSingleItem(topic, it))
		msg.ReplyMarkup = singleKeyboard(it.ID)
		_, _ = a.Bot.Send(msg)
	}
}

func singleKeyboard(id int64) tgbotapi.InlineKeyboardMarkup {
	btn := tgbotapi.NewInlineKeyboardButtonData("✅", fmt.Sprintf("done:%d", id))
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(btn))
}

func formatSingleItem(topic string, it Item) string {
	switch topic {
	case TopicTasks:
		return fmt.Sprintf("ЗАДАЧА #%d: %s", it.ID, it.Text)
	case TopicReminders:
		return fmt.Sprintf("НАПОМИНАНИЕ #%d: %s", it.ID, it.Text)
	case TopicShopping:
		return fmt.Sprintf("ПОКУПКА #%d: %s", it.ID, it.Text)
	case TopicBasket:
		return fmt.Sprintf("КОРЗИНА #%d: %s", it.ID, it.Text)
	default:
		return fmt.Sprintf("%s #%d: %s", strings.ToUpper(topic), it.ID, it.Text)
	}
}

func initApp() (*App, error) {
	tzName := envOr("TZ", "Europe/Moscow")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, err
	}

	dbPath := envOr("DB_PATH", "gtd.db")
	store, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}

	token := mustEnv("BOT_TOKEN")
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	ttlMin, _ := strconv.Atoi(envOr("TTL_MINUTES", "10"))
	ttl := time.Duration(ttlMin) * time.Minute

	return &App{
		Bot:        bot,
		Store:      store,
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

	ctx := context.Background()

	log.Printf("bot started as @%s", app.Bot.Self.UserName)
	if err := app.run(ctx); err != nil {
		log.Fatal(err)
	}
}
