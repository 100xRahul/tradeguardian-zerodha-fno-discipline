package calendar

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNextUnlockSkipsWeekendAndHoliday(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calendar.json")
	data := []byte(`{"year":2026,"holidays":["2026-07-27"],"special_trading_days":[]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	calendar, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	location, _ := time.LoadLocation("Asia/Kolkata")
	friday := time.Date(2026, 7, 24, 14, 0, 0, 0, location)
	want := time.Date(2026, 7, 28, 9, 15, 0, 0, location)
	if got := calendar.NextUnlock(friday); !got.Equal(want) {
		t.Fatalf("NextUnlock() = %s, want %s", got, want)
	}
}

func TestNextUnlockFailsClosedOutsideConfiguredYear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calendar.json")
	if err := os.WriteFile(path, []byte(`{"year":2026,"holidays":[],"special_trading_days":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calendar, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	location, _ := time.LoadLocation("Asia/Kolkata")
	if got := calendar.NextUnlock(time.Date(2026, 12, 31, 12, 0, 0, 0, location)); !got.IsZero() {
		t.Fatalf("NextUnlock() = %s, want zero outside configured year", got)
	}
}

func TestCalendarRejectsWrongYearAndConflictingDates(t *testing.T) {
	tests := []string{
		`{"year":2026,"holidays":["2027-01-01"],"special_trading_days":[]}`,
		`{"year":2026,"holidays":["2026-07-21"],"special_trading_days":["2026-07-21"]}`,
		`{"year":2026,"holidays":["2026-07-21","2026-07-21"],"special_trading_days":[]}`,
	}
	for index, data := range tests {
		path := filepath.Join(t.TempDir(), "calendar.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("case %d: Load() error = nil", index)
		}
	}
}
