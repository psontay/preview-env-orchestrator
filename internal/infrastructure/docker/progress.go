package docker

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var buildLogPatterns = []struct {
	expression  *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`), ""},
	{regexp.MustCompile(`(?i)\b(?:github_pat_[a-z0-9_]+|gh[pousr]_[a-z0-9_]+)\b`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic)[ \t]+[a-z0-9._~+/=-]+`), "$1 [REDACTED]"},
	{regexp.MustCompile(`(?i)([a-z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|GMAIL_PASS)[a-z0-9_]*["']?[ \t]*[=:][ \t]*)(?:"[^"]*"|'[^']*'|[^ \t\r\n;]+)`), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^@\s/]+@`), "${1}[REDACTED]@"},
}

func redactBuildLog(line string) string {
	for _, pattern := range buildLogPatterns {
		line = pattern.expression.ReplaceAllString(line, pattern.replacement)
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, line)
	return line
}

// reportStage emits a heartbeat even while the Docker API has no new output.
// It never changes the provisioning deadline or starts a duplicate build.
func reportStage(ctx context.Context, group, stage string, operation func() error) error {
	start := time.Now()
	log.Printf("[docker] env=%s step=%s state=started", group, stage)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("[docker] env=%s step=%s state=running elapsed=%s", group, stage, time.Since(start).Round(time.Second))
			}
		}
	}()
	err := operation()
	close(stop)
	<-stopped
	state := "completed"
	if err != nil {
		state = "failed"
	}
	log.Printf("[docker] env=%s step=%s state=%s elapsed=%s", group, stage, state, time.Since(start).Round(time.Millisecond))
	return err
}
