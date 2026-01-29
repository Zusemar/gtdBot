package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Scheduler struct {
	bot      *tgbotapi.BotAPI
	store    *Store
	calendar CalendarClient
	tz       *time.Location

	reminderTimes []string // HH:MM in tz
	wipeTime      string   // HH:MM
	morningTime   string   // HH:MM
}

func NewScheduler(bot *tgbotapi.BotAPI, store *Store, cal CalendarClient, tz *time.Location) *Scheduler {
	// Fixed times per your request (Moscow time): 08:00 10:00 14:00 19:00 23:00
	return &Scheduler{
		bot:           bot,
		store:         store,
		calendar:      cal,
		tz:            tz,
		reminderTimes: []string{"08:00", "10:00", "14:00", "19:00", "23:00"},
		wipeTime:      "03:00",
		morningTime:   envOr("MORNING_TIME", "08:00"),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	// Track last fired date+time to avoid duplicates if loop checks multiple times in the same minute.
	lastFired := map[string]string{} // key=kind:time -> date(YYYY-MM-DD)

	ticker := time.NewTicker(15 * time.Second) // small tick; we still match by minute
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().In(s.tz)
			hhmm := now.Format("15:04")
			today := now.Format("2006-01-02")

			// Morning digest (calendar)
			if hhmm == s.morningTime && lastFired["morning:"+hhmm] != today {
				lastFired["morning:"+hhmm] = today
				s.sendMorningDigest(ctx, now)
			}

			// Reminders digest at fixed times
			for _, t := range s.reminderTimes {
				if hhmm == t && lastFired["reminders:"+t] != today {
					lastFired["reminders:"+t] = today
					s.sendReminders(now)
				}
			}

			// Night wipe
			if hhmm == s.wipeTime && lastFired["wipe:"+hhmm] != today {
				lastFired["wipe:"+hhmm] = today
				s.wipeReminders(now)
			}
		}
	}
}

func (s *Scheduler) targetChatID() (int64, bool) {
	// Priority: env CHAT_ID; otherwise read persisted kv chat_id.
	if chatID, ok := parseInt64(strings.TrimSpace(os.Getenv("CHAT_ID"))); ok {
		return chatID, true
	}
	if v, ok := s.store.GetKV("chat_id"); ok {
		return parseInt64(strings.TrimSpace(v))
	}
	return 0, false
}

func parseInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var neg bool
	if strings.HasPrefix(s, "-") {
		neg = true
		s = strings.TrimPrefix(s, "-")
	}
	var n int64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int64(ch-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

func (s *Scheduler) sendMorningDigest(ctx context.Context, now time.Time) {
	chatID, ok := s.targetChatID()
	if !ok {
		log.Printf("scheduler: CHAT_ID not set; skipping morning digest")
		return
	}
	text, err := s.calendar.GetTodaySchedule(ctx, now)
	if err != nil {
		text = fmt.Sprintf("Ошибка чтения календаря: %v", err)
	}
	msg := tgbotapi.NewMessage(chatID, "РАСПИСАНИЕ НА СЕГОДНЯ:\n"+text)
	_, _ = s.bot.Send(msg)
}

func (s *Scheduler) sendReminders(now time.Time) {
	chatID, ok := s.targetChatID()
	if !ok {
		log.Printf("scheduler: CHAT_ID not set and kv.chat_id missing; skipping reminders")
		return
	}
	items, err := s.store.ListActive(chatID, TopicReminders)
	if err != nil {
		log.Printf("scheduler: list reminders error: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	for _, it := range items {
		msg := tgbotapi.NewMessage(chatID, formatSingleReminder(it))
		msg.ReplyMarkup = singleReminderKeyboard(it.ID)
		_, _ = s.bot.Send(msg)
	}
}

func (s *Scheduler) wipeReminders(now time.Time) {
	chatID, ok := s.targetChatID()
	if !ok {
		log.Printf("scheduler: CHAT_ID not set; skipping wipe")
		return
	}
	if err := s.store.DeleteAllReminders(chatID); err != nil {
		log.Printf("scheduler: wipe reminders error: %v", err)
		return
	}
	msg := tgbotapi.NewMessage(chatID, "НАПОМИНАНИЯ ОЧИЩЕНЫ (ночной вайп).")
	_, _ = s.bot.Send(msg)
}
