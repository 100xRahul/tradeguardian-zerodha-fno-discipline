package calendar

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type fileData struct {
	Year           int      `json:"year"`
	Holidays       []string `json:"holidays"`
	SpecialTrading []string `json:"special_trading_days"`
}

type Calendar struct {
	location *time.Location
	year     int
	holidays map[string]struct{}
	special  map[string]struct{}
}

func Load(path string) (*Calendar, error) {
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return nil, fmt.Errorf("load Asia/Kolkata timezone: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trading calendar: %w", err)
	}
	var decoded fileData
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode trading calendar: %w", err)
	}
	if decoded.Year < 2000 {
		return nil, fmt.Errorf("trading calendar year is required")
	}
	calendar := &Calendar{location: location, year: decoded.Year, holidays: map[string]struct{}{}, special: map[string]struct{}{}}
	for _, value := range decoded.Holidays {
		if err := validateDate(value, decoded.Year); err != nil {
			return nil, fmt.Errorf("holiday %q: %w", value, err)
		}
		if _, exists := calendar.holidays[value]; exists {
			return nil, fmt.Errorf("holiday %q is duplicated", value)
		}
		calendar.holidays[value] = struct{}{}
	}
	for _, value := range decoded.SpecialTrading {
		if err := validateDate(value, decoded.Year); err != nil {
			return nil, fmt.Errorf("special trading day %q: %w", value, err)
		}
		if _, exists := calendar.special[value]; exists {
			return nil, fmt.Errorf("special trading day %q is duplicated", value)
		}
		if _, conflict := calendar.holidays[value]; conflict {
			return nil, fmt.Errorf("date %q cannot be both a holiday and a special trading day", value)
		}
		calendar.special[value] = struct{}{}
	}
	return calendar, nil
}

func validateDate(value string, year int) error {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return fmt.Errorf("must be YYYY-MM-DD")
	}
	if parsed.Year() != year {
		return fmt.Errorf("must be within calendar year %d", year)
	}
	return nil
}

func (c *Calendar) TradingDate(value time.Time) string {
	return value.In(c.location).Format("2006-01-02")
}

func (c *Calendar) IsTradingDay(value time.Time) bool {
	local := value.In(c.location)
	if local.Year() != c.year {
		return false
	}
	date := local.Format("2006-01-02")
	if _, ok := c.special[date]; ok {
		return true
	}
	if _, ok := c.holidays[date]; ok {
		return false
	}
	return local.Weekday() != time.Saturday && local.Weekday() != time.Sunday
}

func (c *Calendar) NextUnlock(after time.Time) time.Time {
	local := after.In(c.location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 9, 15, 0, 0, c.location).AddDate(0, 0, 1)
	if day.Year() != c.year {
		return time.Time{}
	}
	for !c.IsTradingDay(day) {
		day = day.AddDate(0, 0, 1)
		if day.Year() != c.year {
			return time.Time{}
		}
	}
	return day
}
