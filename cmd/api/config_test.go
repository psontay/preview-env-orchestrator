package main

import (
	"strings"
	"testing"
	"time"
)

func TestRealModeRequiresSecretsAndTunnel(t *testing.T) {
	t.Setenv("DOCKER_DRIVER", "real")
	t.Setenv("GITHUB_DRIVER", "real")
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("GITHUB_WEBHOOK_SECRET", strings.Repeat("s", 64))
	t.Setenv("PUBLIC_BASE_URL", "https://demo.trycloudflare.com")
	t.Setenv("PREVIEW_TTL", "5m")
	t.Setenv("DELETION_GRACE_PERIOD", "1m")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TTL != 5*time.Minute || cfg.Grace != time.Minute {
		t.Fatal("wrong demo timing")
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("real mode must not silently disable HMAC")
	}
}

func TestInvalidDurationsFailBeforeStartingServices(t *testing.T) {
	t.Setenv("DOCKER_DRIVER", "mock")
	t.Setenv("GITHUB_DRIVER", "mock")
	t.Setenv("CLEANUP_INTERVAL", "0s")
	if _, err := loadConfig(); err == nil {
		t.Fatal("zero cleanup ticker would panic")
	}
}
