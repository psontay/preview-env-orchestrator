package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	TTL, Grace, Sweep, ProvisionTimeout, ReadyTimeout time.Duration
	Quota                                             int
	Repository, Actor, Secret, BaseURL                string
}

func loadConfig() (config, error) {
	c := config{Repository: getenv("PREVIEW_REPOSITORY", "psontay/todolist"), Secret: os.Getenv("GITHUB_WEBHOOK_SECRET"), BaseURL: getenv("PUBLIC_BASE_URL", "http://localhost:18080")}
	c.Actor = getenv("PREVIEW_ALLOWED_ACTOR", strings.Split(c.Repository, "/")[0])
	var err error
	for _, item := range []struct {
		key, def string
		dst      *time.Duration
	}{
		{"PREVIEW_TTL", "24h", &c.TTL}, {"DELETION_GRACE_PERIOD", "10m", &c.Grace}, {"CLEANUP_INTERVAL", "5s", &c.Sweep},
		{"PROVISION_TIMEOUT", "20m", &c.ProvisionTimeout}, {"PREVIEW_READY_TIMEOUT", "3m", &c.ReadyTimeout},
	} {
		*item.dst, err = time.ParseDuration(getenv(item.key, item.def))
		if err != nil || *item.dst <= 0 {
			return c, fmt.Errorf("%s must be a positive duration", item.key)
		}
	}
	c.Quota, err = strconv.Atoi(getenv("PREVIEW_QUOTA", "5"))
	if err != nil || c.Quota <= 0 {
		return c, fmt.Errorf("PREVIEW_QUOTA must be positive")
	}
	if getenv("DOCKER_DRIVER", "mock") == "real" || getenv("GITHUB_DRIVER", "mock") == "real" {
		if len(c.Secret) < 32 || strings.Contains(c.Secret, "CHANGE_ME") {
			return c, fmt.Errorf("GITHUB_WEBHOOK_SECRET must contain at least 32 characters")
		}
		if token := os.Getenv("GITHUB_TOKEN"); token == "" || strings.Contains(token, "CHANGE_ME") {
			return c, fmt.Errorf("GITHUB_TOKEN is required")
		}
		parsed, err := url.Parse(c.BaseURL)
		if err != nil || parsed.Host == "" || parsed.Scheme != "https" || strings.Contains(c.BaseURL, "CHANGE_ME") {
			return c, fmt.Errorf("PUBLIC_BASE_URL must be your HTTPS tunnel URL")
		}
	}
	return c, nil
}
