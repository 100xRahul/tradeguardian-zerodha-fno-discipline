package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"tradeguardian/internal/broker"
	tradingcalendar "tradeguardian/internal/calendar"
	"tradeguardian/internal/httpapi"
	"tradeguardian/internal/service"
	"tradeguardian/internal/sessioncache"
	"tradeguardian/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Printf("level=error message=%q error=%q", "TradeGuardian stopped", err)
		os.Exit(1)
	}
}

func run() error {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	config, err := loadConfig()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config.databasePath), 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	cal, err := tradingcalendar.Load(config.calendarPath)
	if err != nil {
		return err
	}
	kite, err := broker.NewKite(config.apiKey, config.apiSecret)
	if err != nil {
		return err
	}
	database, err := store.Open(config.databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := os.Chmod(config.databasePath, 0o600); err != nil {
		return fmt.Errorf("protect database file: %w", err)
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := service.New(rootCtx, kite, database, cal, logger, time.Now)
	if err != nil {
		return err
	}
	tokenCache, err := sessioncache.NewFile(config.sessionCachePath, time.Now)
	if err != nil {
		return err
	}
	app.SetSessionCache(tokenCache)
	if err := app.RestoreCachedSession(rootCtx); err != nil {
		return fmt.Errorf("restore cached Kite session: %w", err)
	}
	web, err := httpapi.New(app, httpapi.Config{PublicOrigin: config.publicOrigin})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(config.port),
		Handler:           web.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go app.Run(rootCtx)
	serverErrors := make(chan error, 1)
	go func() {
		logger.Printf("level=info message=%q url=%q mode=%q", "TradeGuardian listening", config.publicOrigin, "production")
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case <-rootCtx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve TradeGuardian: %w", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	web.CloseLiveStreams()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown TradeGuardian: %w", err)
	}
	return nil
}

type config struct {
	apiKey           string
	apiSecret        string
	port             int
	databasePath     string
	sessionCachePath string
	calendarPath     string
	publicOrigin     string
}

func loadConfig() (config, error) {
	result := config{
		apiKey: os.Getenv("KITE_API_KEY"), apiSecret: os.Getenv("KITE_API_SECRET"),
		port:         8080,
		databasePath: envOr("TRADEGUARDIAN_DB", "data/tradeguardian.db"),
		calendarPath: envOr("TRADING_CALENDAR", "config/trading_holidays_2026.json"),
	}
	result.sessionCachePath = envOr("TRADEGUARDIAN_SESSION_CACHE", filepath.Join(filepath.Dir(result.databasePath), "kite-session.json"))
	if value := os.Getenv("TRADEGUARDIAN_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1024 || port > 65535 {
			return result, fmt.Errorf("TRADEGUARDIAN_PORT must be between 1024 and 65535")
		}
		result.port = port
	}
	defaultOrigin := "http://127.0.0.1:" + strconv.Itoa(result.port)
	result.publicOrigin = envOr("TRADEGUARDIAN_PUBLIC_ORIGIN", defaultOrigin)
	return result, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
