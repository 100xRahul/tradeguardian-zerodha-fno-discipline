package sessioncache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tradeguardian/internal/domain"
)

func TestFileRoundTripIsOwnerOnlyAndExpiresAtNextSixIST(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 10, 30, 0, 0, ist)
	path := filepath.Join(t.TempDir(), "kite-session.json")
	cache, err := NewFile(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	session := domain.Session{UserID: "AB1234", AccessToken: "sensitive-token"}
	if err := cache.Save(context.Background(), session, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache permissions = %o", info.Mode().Perm())
	}
	loaded, err := cache.Load(context.Background())
	if err != nil || loaded != session {
		t.Fatalf("loaded=%#v error=%v", loaded, err)
	}

	now = time.Date(2026, 7, 22, 6, 0, 0, 0, ist)
	if _, err := cache.Load(context.Background()); !errors.Is(err, domain.ErrNoCachedSession) {
		t.Fatalf("expired Load() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired cache still exists: %v", err)
	}
}

func TestFileRejectsCacheReadableByOtherUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kite-session.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cache, err := NewFile(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(context.Background()); err == nil {
		t.Fatal("Load() accepted a cache readable by group/other")
	}
}

func TestFileRejectsTrailingOrUnknownData(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 30, 0, 0, time.FixedZone("IST", 5*60*60+30*60))
	for name, contents := range map[string]string{
		"trailing": `{"version":1,"user_id":"AB","access_token":"token","issued_at":"2026-07-21T10:30:00+05:30","expires_at":"2026-07-22T06:00:00+05:30"}{}`,
		"unknown":  `{"version":1,"user_id":"AB","access_token":"token","issued_at":"2026-07-21T10:30:00+05:30","expires_at":"2026-07-22T06:00:00+05:30","unexpected":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kite-session.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			cache, err := NewFile(path, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Load(context.Background()); err == nil {
				t.Fatal("Load() accepted invalid JSON cache")
			}
		})
	}
}
