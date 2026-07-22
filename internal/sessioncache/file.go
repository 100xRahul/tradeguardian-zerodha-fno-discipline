package sessioncache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tradeguardian/internal/domain"
)

const cacheVersion = 1

type File struct {
	path string
	now  func() time.Time
	ist  *time.Location
}

type record struct {
	Version     int       `json:"version"`
	UserID      string    `json:"user_id"`
	AccessToken string    `json:"access_token"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func NewFile(path string, now func() time.Time) (*File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("session cache path is required")
	}
	if now == nil {
		now = time.Now
	}
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return nil, fmt.Errorf("load Asia/Kolkata: %w", err)
	}
	return &File{path: path, now: now, ist: ist}, nil
}

func (f *File) Load(ctx context.Context) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	pathInfo, err := os.Lstat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.Session{}, domain.ErrNoCachedSession
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("inspect Kite session cache path: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return domain.Session{}, fmt.Errorf("Kite session cache must not be a symbolic link")
	}
	file, err := os.Open(f.path)
	if err != nil {
		return domain.Session{}, fmt.Errorf("open Kite session cache: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return domain.Session{}, fmt.Errorf("inspect Kite session cache: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 16<<10 {
		return domain.Session{}, fmt.Errorf("Kite session cache must be a regular owner-only file")
	}
	var cached record
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cached); err != nil {
		return domain.Session{}, fmt.Errorf("decode Kite session cache: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return domain.Session{}, err
	}
	if cached.Version != cacheVersion || strings.TrimSpace(cached.AccessToken) == "" || cached.IssuedAt.IsZero() || cached.ExpiresAt.IsZero() || !cached.ExpiresAt.After(cached.IssuedAt) {
		return domain.Session{}, fmt.Errorf("Kite session cache metadata is invalid")
	}
	if !f.now().Before(cached.ExpiresAt) {
		if err := f.Delete(ctx); err != nil {
			return domain.Session{}, err
		}
		return domain.Session{}, domain.ErrNoCachedSession
	}
	return domain.Session{UserID: cached.UserID, AccessToken: cached.AccessToken}, ctx.Err()
}

func (f *File) Save(ctx context.Context, session domain.Session, issuedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(session.AccessToken) == "" || issuedAt.IsZero() {
		return fmt.Errorf("cannot cache an empty or undated Kite session")
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("create Kite session cache directory: %w", err)
	}
	cached := record{
		Version: cacheVersion, UserID: session.UserID, AccessToken: session.AccessToken,
		IssuedAt: issuedAt, ExpiresAt: nextKiteExpiry(issuedAt, f.ist),
	}
	temporary, err := os.CreateTemp(filepath.Dir(f.path), ".kite-session-*")
	if err != nil {
		return fmt.Errorf("create temporary Kite session cache: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary Kite session cache: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(cached); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode Kite session cache: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Kite session cache: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Kite session cache: %w", err)
	}
	if err := os.Rename(temporaryPath, f.path); err != nil {
		return fmt.Errorf("install Kite session cache: %w", err)
	}
	keep = true
	return ctx.Err()
}

func (f *File) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete Kite session cache: %w", err)
	}
	return nil
}

func nextKiteExpiry(issuedAt time.Time, ist *time.Location) time.Time {
	local := issuedAt.In(ist)
	next := local.AddDate(0, 0, 1)
	return time.Date(next.Year(), next.Month(), next.Day(), 6, 0, 0, 0, ist)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing Kite session cache data: %w", err)
	}
	return fmt.Errorf("Kite session cache contains trailing data")
}
