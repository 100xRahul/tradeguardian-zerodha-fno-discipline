package main

import "testing"

func TestProductionConfigurationDefaultsRemainUnchanged(t *testing.T) {
	for _, key := range []string{"TRADEGUARDIAN_PORT", "TRADEGUARDIAN_PUBLIC_ORIGIN", "TRADEGUARDIAN_DB", "TRADEGUARDIAN_SESSION_CACHE"} {
		t.Setenv(key, "")
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.port != 8080 || config.publicOrigin != "http://127.0.0.1:8080" || config.databasePath != "data/tradeguardian.db" || config.sessionCachePath != "data/kite-session.json" {
		t.Fatalf("production config = %#v", config)
	}
}
