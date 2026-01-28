package main

// Bot implementing a simple GTD-style assistant with topics (tasks, reminders,
// shopping basket) and a minimal scheduler.  This code is designed as a
// starting point for your Telegram bot.  It persists items into a SQLite
// database, maintains an in‑memory state per chat to know which topic is
// currently active, and periodically sends reminders and performs a daily
// cleanup.  The reminder times and the TTL for resetting the state are
// configurable via constants at the top of the file.  Google Calendar
// integration is stubbed out; replace the placeholder with calls to the
// Google Calendar API if desired.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "modernc.org/sqlite"
)

// Configuration constants.  Adjust these values to suit your workflow.

// Bot token.  Set this environment variable before running the bot.  For
// example: export BOT_TOKEN=123456:ABCDEF... .  The program will exit if
// BOT_TOKEN is not provided.
const envBotToken = "BOT_TOKEN"

// Path to the SQLite database file.  By default the bot uses a file named
// gtd.db in the current working directory.  You can override this via the
// DB_PATH environment variable.
const envDBPath = "DB_PATH"

// Time zone used for scheduling reminders and daily cleanup.  The default
// value corresponds to Moscow time.  Override via TZ env var if you live in
// another time zone.
const envTimeZone = "TZ"

// ReminderTimes defines the local times at which reminders should be sent.
// Times are specified in 24‑hour "HH:MM" format.  You can adjust these
// strings to change the notification schedule.  The times here reflect the
// user's request to send reminders at 08:00, 10:00, 14:00, 19:00 and 23:00.
var ReminderTimes = []string{
	"08:00",
	"10:00",
	"14:00",
	"19:00",
	"23:00",
}

// TTLMinutes defines how long (in minutes) a selected topic remains active
// after the last user interaction.  When the TTL expires the bot defaults
// back to the basket topic.  The user asked for a TTL of 10 minutes.
const TTLMinutes = 10

// DailyCleanupHour defines the hour at which reminders should be wiped.  At
// this time the bot will delete all existing reminder items.  The minute is
// fixed at zero.  Adjust the hour if you prefer another cleanup time.  The
// user wanted reminders older than one day to be removed at night; we choose
// 03:00 local time for the cleanup.
const DailyCleanupHour = 3

// Topics represent the different lists supported by the bot.  Basket is the
// default topic; when no topic is selected all incoming messages go to the
// basket.  The textual representation is stored in the database.  Use
// lowercase strings for consistency.
const (
	TopicBasket    = "basket"
	TopicTasks     = "tasks"
	TopicReminders = "reminders"
	TopicShopping  = "shopping"
)

// State holds per‑chat metadata such as the current topic and the time of
// last activity.  It lives in memory only.  If the bot is restarted or
// crashes, the state will be reset to the default topic.
type State struct {
	Topic        string
	LastActivity time.Time
}

// Item represents a single entry in the database.  Only the fields used by
// the bot at runtime are defined here.  Additional fields can be added as
// needed (for example, to track last sent timestamps for reminders).
type Item struct {
	ID        int64
	ChatID    int64
	Topic     string
	Text      string
	CreatedAt time.Time
	Status    int // 0=active, 1=deleted
}

// states maps chat IDs to State instances.  Access to this map is not
// synchronized because the bot runs a single goroutine processing updates
// sequentially.  If you refactor to use multiple goroutines, guard this map
// with a mutex.
var states = make(map[int64]*State)

// lastRemindersSent tracks the date on which reminders were last sent for
// each schedule time.  The key is "HH:MM" and the value is a date string
// "YYYY‑MM‑DD".  This prevents sending reminders multiple times in the
// same day when checking every minute.  It's accessed from the scheduler
// goroutine only and does not need synchronization.
var lastRemindersSent = make(map[string]string)

func main() {
	// Read configuration from the environment.
	botToken := os.Getenv(envBotToken)
	if botToken == "" {
		log.Fatalf("%s is not set; please provide your Telegram bot token", envBotToken)
	}
	dbPath := os.Getenv(envDBPath)
	if dbPath == "" {
		dbPath = "gtd.db"
	}
	tz := os.Getenv(envTimeZone)
	if tz == "" {
		// Default to Moscow time as requested by the user.
		tz = "Europe/Moscow"
	}

	// Parse the location for scheduling.  If the time zone identifier is
	// invalid the program will abort.
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Fatalf("invalid time zone %q: %v", tz, err)
	}

	// Connect to SQLite database and perform a migration to create the items
	// table.  Using modernc.org/sqlite eliminates the need for CGO.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	if err := migrate(db); err != nil {
		log.Fatalf("failed to run migration: %v", err)
	}

	// Start the Telegram bot API.
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("failed to create Telegram bot: %v", err)
	}
	// Optionally enable verbose logging of all requests and responses.
	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	// Launch scheduler goroutine for sending reminders and performing cleanup.
	go scheduler(bot, db, loc)

	// Configure update polling.  We use long polling with a 60 second timeout.
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60
	updates := bot.GetUpdatesChan(updateConfig)

	// Main event loop: handle incoming messages and callback queries.
	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallback(bot, db, update.CallbackQuery)
			continue
		}
		if update.Message != nil {
			handleMessage(bot, db, update.Message, loc)
			continue
		}
	}
}

// migrate creates the items table if it does not already exist.  The table
// stores items for all chats.  Each item belongs to a topic and may be
// marked as deleted when it has been handled.  Additional columns can be
// added without breaking existing bots as long as they allow NULLs or have
// default values.
func migrate(db *sql.DB) error {
	const createTable = `
    CREATE TABLE IF NOT EXISTS items (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        chat_id INTEGER NOT NULL,
        topic TEXT NOT NULL,
        text TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        status INTEGER NOT NULL DEFAULT 0
    );
    CREATE INDEX IF NOT EXISTS idx_items_chat_topic_status ON items(chat_id, topic, status);
    `
	_, err := db.Exec(createTable)
	return err
}

// getState returns the State associated with chatID.  If no state exists a
// new one is created with the default topic.  The state's LastActivity is
// updated to the current time whenever it is returned.
func getState(chatID int64) *State {
	s, ok := states[chatID]
	if !ok {
		s = &State{Topic: TopicBasket}
		states[chatID] = s
	}
	return s
}

// resetTopicIfExpired resets the chat's topic to the basket if the last
// activity occurred more than TTLMinutes ago.  After resetting, the
// LastActivity timestamp is updated to now.
func resetTopicIfExpired(s *State, now time.Time) {
	if now.Sub(s.LastActivity) > time.Duration(TTLMinutes)*time.Minute {
		s.Topic = TopicBasket
	}
	s.LastActivity = now
}

// handleMessage processes an incoming message.  It recognises commands,
// text from the reply keyboard buttons and arbitrary text.  Commands are
// prefixed with '/'; buttons are plain text matching the button labels.
func handleMessage(bot *tgbotapi.BotAPI, db *sql.DB, msg *tgbotapi.Message, loc *time.Location) {
	chatID := msg.Chat.ID
	now := time.Now().In(loc)
	s := getState(chatID)
	// Reset topic on inactivity.
	resetTopicIfExpired(s, now)

	// Slash commands override other processing.
	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			handleStart(bot, msg)
		case "menu":
			handleMenu(bot, msg)
		default:
			// Unknown commands are ignored gracefully.
		}
		return
	}

	// Reply keyboard button presses are treated as plain text.  Check for
	// known labels and switch topics accordingly.  The menu button resets
	// to the basket topic.
	switch strings.ToLower(msg.Text) {
	case "tasks":
		s.Topic = TopicTasks
		s.LastActivity = now
		reply := tgbotapi.NewMessage(chatID, "Текущий раздел: задачи")
		reply.ReplyMarkup = defaultKeyboard()
		bot.Send(reply)
		return
	case "reminders", "напоминания":
		s.Topic = TopicReminders
		s.LastActivity = now
		reply := tgbotapi.NewMessage(chatID, "Текущий раздел: напоминания")
		reply.ReplyMarkup = defaultKeyboard()
		bot.Send(reply)
		return
	case "shopping", "покупки":
		s.Topic = TopicShopping
		s.LastActivity = now
		reply := tgbotapi.NewMessage(chatID, "Текущий раздел: покупки")
		reply.ReplyMarkup = defaultKeyboard()
		bot.Send(reply)
		return
	case "basket", "корзина":
		s.Topic = TopicBasket
		s.LastActivity = now
		reply := tgbotapi.NewMessage(chatID, "Текущий раздел: корзина")
		reply.ReplyMarkup = defaultKeyboard()
		bot.Send(reply)
		return
	case "menu", "меню":
		s.Topic = TopicBasket
		s.LastActivity = now
		reply := tgbotapi.NewMessage(chatID, "Главное меню")
		reply.ReplyMarkup = defaultKeyboard()
		bot.Send(reply)
		return
	}

	// For any other text we treat it as content to be stored in the
	// currently selected topic.  Only non‑empty messages are stored.
	if strings.TrimSpace(msg.Text) == "" {
		return
	}
	if err := insertItem(db, chatID, s.Topic, msg.Text, now); err != nil {
		log.Printf("failed to store item: %v", err)
		return
	}
	// Send confirmation to the user indicating which list the message went to.
	ack := fmt.Sprintf("Добавил сообщение в %s", humanTopic(s.Topic))
	reply := tgbotapi.NewMessage(chatID, ack)
	reply.ReplyMarkup = defaultKeyboard()
	bot.Send(reply)
}

// handleStart sends a welcome message and resets the chat's topic to the
// basket.  It also displays the main menu keyboard.
func handleStart(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	s := getState(chatID)
	s.Topic = TopicBasket
	s.LastActivity = time.Now()
	text := "Привет! Это GTD бот. Вы можете добавлять задачи, напоминания и покупки. " +
		"Используйте кнопки для выбора раздела."
	reply := tgbotapi.NewMessage(chatID, text)
	reply.ReplyMarkup = defaultKeyboard()
	bot.Send(reply)
}

// handleMenu resets the topic to the basket and re‑shows the main menu.
func handleMenu(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	s := getState(chatID)
	s.Topic = TopicBasket
	s.LastActivity = time.Now()
	reply := tgbotapi.NewMessage(chatID, "Главное меню")
	reply.ReplyMarkup = defaultKeyboard()
	bot.Send(reply)
}

// insertItem stores a new item in the database.  It records the chatID, the
// current topic, the text of the message and the creation timestamp.  The
// status is always set to 0 (active).
func insertItem(db *sql.DB, chatID int64, topic, text string, at time.Time) error {
	if topic == "" {
		return errors.New("topic is empty")
	}
	_, err := db.Exec(
		"INSERT INTO items(chat_id, topic, text, created_at, status) VALUES(?, ?, ?, ?, 0)",
		chatID,
		topic,
		text,
		at.Unix(),
	)
	return err
}

// loadActiveItems loads all active items for a given chat and topic.  Deleted
// items (status=1) are ignored.  This function returns a slice of Item
// structs.  If no items exist, it returns an empty slice and nil error.
func loadActiveItems(db *sql.DB, chatID int64, topic string) ([]Item, error) {
	rows, err := db.Query(
		"SELECT id, text, created_at FROM items WHERE chat_id = ? AND topic = ? AND status = 0 ORDER BY id",
		chatID,
		topic,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		var ts int64
		if err := rows.Scan(&it.ID, &it.Text, &ts); err != nil {
			return nil, err
		}
		it.ChatID = chatID
		it.Topic = topic
		it.CreatedAt = time.Unix(ts, 0)
		items = append(items, it)
	}
	return items, rows.Err()
}

// deleteItem marks an item as deleted.  This is used when the user taps
// the ✅ button in a reminder message.  If no rows are affected the ID may
// be invalid.
func deleteItem(db *sql.DB, chatID, id int64) error {
	_, err := db.Exec("UPDATE items SET status = 1 WHERE chat_id = ? AND id = ?", chatID, id)
	return err
}

// handleCallback processes inline keyboard callback queries.  For now we
// support only deletion of reminder items via data in the form "done:<id>".
func handleCallback(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery) {
	// Acknowledge the callback to remove the loading animation.
	answer := tgbotapi.NewCallback(cq.ID, "")
	bot.Request(answer)

	// Parse the callback data.  Expect format "done:<id>".
	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) != 2 || parts[0] != "done" {
		return
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return
	}
	chatID := cq.Message.Chat.ID
	// Mark the item as deleted.
	if err := deleteItem(db, chatID, id); err != nil {
		log.Printf("failed to delete reminder item %d: %v", id, err)
	}
	// Optionally edit the message to reflect the deletion.  For simplicity
	// this implementation does not modify the original message; the next
	// reminder broadcast will omit deleted items.
}

// scheduler runs in a separate goroutine.  It wakes up every minute to
// perform scheduled tasks: sending reminders at specified times and cleaning
// up reminders nightly.  It also sends a daily Google Calendar digest at
// morning time.  Replace the sendCalendarDigest function with a real call
// to Google Calendar if you wish to integrate your agenda.
func scheduler(bot *tgbotapi.BotAPI, db *sql.DB, loc *time.Location) {
	// Determine the hour and minute components of the configured reminder times.
	type hm struct{ hour, minute int }
	var schedule []hm
	for _, s := range ReminderTimes {
		p := strings.Split(s, ":")
		if len(p) != 2 {
			log.Printf("invalid reminder time %q; skipping", s)
			continue
		}
		h, _ := strconv.Atoi(p[0])
		m, _ := strconv.Atoi(p[1])
		schedule = append(schedule, hm{h, m})
	}
	// Morning digest time: fixed at 08:00 for now.  Adjust if needed.
	digestHour := 8
	digestMinute := 0

	// Variables to track last cleanup and last digest.  Using date strings
	// prevents multiple runs within the same day.
	lastCleanupDate := ""
	lastDigestDate := ""
	for {
		now := time.Now().In(loc)
		// Reminders: check each configured time.
		dateKey := now.Format("2006-01-02")
		for _, t := range schedule {
			if now.Hour() == t.hour && now.Minute() == t.minute {
				key := fmt.Sprintf("%02d:%02d", t.hour, t.minute)
				if lastRemindersSent[key] != dateKey {
					// Send reminders to all chats currently known.  If you want
					// to limit this to a single user you can instead call
					// sendReminders for that chat only.
					for chatID := range states {
						sendReminders(bot, db, chatID, loc)
					}
					lastRemindersSent[key] = dateKey
				}
			}
		}
		// Daily cleanup at DailyCleanupHour:00.
		if now.Hour() == DailyCleanupHour && now.Minute() == 0 {
			if lastCleanupDate != dateKey {
				cleanupReminders(db)
				lastCleanupDate = dateKey
			}
		}
		// Morning digest at digestHour:digestMinute.
		if now.Hour() == digestHour && now.Minute() == digestMinute {
			if lastDigestDate != dateKey {
				for chatID := range states {
					sendCalendarDigest(bot, chatID, loc)
				}
				lastDigestDate = dateKey
			}
		}
		// Sleep until the next minute.
		time.Sleep(time.Minute)
	}
}

// sendReminders fetches all active reminders for a chat and sends them as a
// single message.  The message includes numbered lines and an inline
// keyboard with a ✅ button for each reminder, allowing the user to mark it
// as done.  If no reminders exist, nothing is sent.
func sendReminders(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, loc *time.Location) {
	items, err := loadActiveItems(db, chatID, TopicReminders)
	if err != nil {
		log.Printf("failed to load reminders: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	// Build the message body.
	var sb strings.Builder
	sb.WriteString("Напоминания:\n")
	buttons := make([]tgbotapi.InlineKeyboardButton, 0, len(items))
	for i, item := range items {
		sb.WriteString(fmt.Sprintf("%d) %s\n", i+1, item.Text))
		// Each button holds the item ID so the callback can delete it.
		btnText := fmt.Sprintf("✅ %d", i+1)
		callbackData := fmt.Sprintf("done:%d", item.ID)
		buttons = append(buttons, tgbotapi.InlineKeyboardButton{
			Text:         btnText,
			CallbackData: &callbackData,
		})
	}
	// Arrange buttons in a single row.  If you prefer multiple rows you can
	// distribute the buttons into several rows of the InlineKeyboardMarkup.
	markup := tgbotapi.NewInlineKeyboardMarkup(buttons)
	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ReplyMarkup = markup
	bot.Send(msg)
}

// cleanupReminders deletes all reminder items from the database.  It runs
// nightly to prevent reminders from accumulating beyond a day.  Adjust the
// SQL statement if you want to archive rather than delete.
func cleanupReminders(db *sql.DB) {
	_, err := db.Exec("DELETE FROM items WHERE topic = ?", TopicReminders)
	if err != nil {
		log.Printf("failed to delete reminders: %v", err)
	}
}

// sendCalendarDigest sends a daily digest of the user's schedule.  Replace
// the body of this function with an actual call to the Google Calendar API.
// The user requested to receive their schedule in the morning.  For now we
// send a placeholder message.  If you integrate Google Calendar, you can
// remove the placeholder and build the digest from the events returned by
// the API.
func sendCalendarDigest(bot *tgbotapi.BotAPI, chatID int64, loc *time.Location) {
	// Placeholder implementation.  To integrate Google Calendar:
	// 1. Authorise with the Calendar API (OAuth2 or service account).
	// 2. Query events for today using the time zone in `loc`.
	// 3. Format the events into a message and send it here.
	msg := tgbotapi.NewMessage(chatID, "Ваше расписание на сегодня из Google Calendar (placeholder)")
	bot.Send(msg)
}

// defaultKeyboard returns the reply keyboard markup used for the main menu.
// The keyboard consists of buttons for each topic and a menu button.  The
// labels are localised in both English and Russian to illustrate how you
// could support multiple languages; the bot simply compares lowercase text
// when matching button presses.
func defaultKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Tasks"),
			tgbotapi.NewKeyboardButton("Reminders"),
			tgbotapi.NewKeyboardButton("Shopping"),
			tgbotapi.NewKeyboardButton("Basket"),
			tgbotapi.NewKeyboardButton("Menu"),
		),
	)
}

// humanTopic maps internal topic identifiers to human‑readable names.  Use
// this when acknowledging to the user that a message was added to a list.
func humanTopic(topic string) string {
	switch topic {
	case TopicTasks:
		return "задачи"
	case TopicReminders:
		return "напоминания"
	case TopicShopping:
		return "покупки"
	case TopicBasket:
		return "корзину"
	default:
		return topic
	}
}
