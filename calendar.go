package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// CalendarClient is a tiny interface the scheduler uses.
// For MVP, we keep it minimal: get today's schedule as a preformatted text.
type CalendarClient interface {
	GetTodaySchedule(ctx context.Context, now time.Time) (string, error)
}

// googleCalendarClient is intentionally minimal here.
// полноценный OAuth2 + Calendar API можно добавить позже.
type googleCalendarClient struct {
	enabled bool
	calendarID string
	tz *time.Location
}

func NewGoogleCalendarClientFromEnv(tz *time.Location) (CalendarClient, error) {
	// If GCAL_DISABLED=true or missing config -> return disabled client
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GCAL_DISABLED")), "true") {
		return &googleCalendarClient{enabled:false, tz: tz}, nil
	}
	calID := strings.TrimSpace(os.Getenv("GCAL_CALENDAR_ID"))
	// For real integration you'd also require OAuth credentials.
	// In this MVP we treat missing calendar id as "disabled".
	if calID == "" {
		return &googleCalendarClient{enabled:false, tz: tz}, nil
	}
	return &googleCalendarClient{
		enabled: true,
		calendarID: calID,
		tz: tz,
	}, nil
}

func (c *googleCalendarClient) GetTodaySchedule(ctx context.Context, now time.Time) (string, error) {
	if !c.enabled {
		return "Расписание из Google Calendar не настроено (GCAL_CALENDAR_ID не задан).", nil
	}

	// TODO: Реальная интеграция:
	// 1) OAuth2 / service account
	// 2) calendar/v3 Events.List with timeMin/timeMax for "today" in tz
	// 3) форматирование событий
	//
	// Пока возвращаем заглушку, чтобы остальная система работала.
	return fmt.Sprintf("Расписание на сегодня (%s):\n(заглушка) календарь=%s", now.In(c.tz).Format("2006-01-02"), c.calendarID), nil
}

var ErrCalendarNotConfigured = errors.New("calendar not configured")
