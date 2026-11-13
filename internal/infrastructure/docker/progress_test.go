package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuildOutputRedactsCredentials(t *testing.T) {
	for _, item := range []struct{ line, secret string }{
		{"JWT_SECRET=private-value", "private-value"},
		{`{"password":"private-value"}`, "private-value"},
		{"GMAIL_PASS='private-value'", "private-value"},
		{"Authorization: Bearer private-value", "private-value"},
		{"https://user:private-value@example.com/archive", "private-value"},
		{"github_pat_1234567890abcdefghijklmnopqrstuvwxyz", "github_pat_1234567890abcdefghijklmnopqrstuvwxyz"},
	} {
		redacted := redactBuildLog(item.line)
		if strings.Contains(redacted, item.secret) || !strings.Contains(redacted, "[REDACTED]") {
			t.Fatalf("credential was not redacted for test case")
		}
	}
	if got := redactBuildLog("\x1b[31m[INFO] Compiling 25 files\x1b[0m"); got != "[INFO] Compiling 25 files" {
		t.Fatalf("build progress lost: %q", got)
	}
}

func TestDockerStreamReportsBuildOutputAndSuppressesRepeatedLayerStatus(t *testing.T) {
	input := `{"stream":"Step 1/2 : FROM eclipse-temurin\n"}
{"id":"layer","status":"Downloading"}
{"id":"layer","status":"Downloading"}
{"stream":"[INFO] Compiling sources\n"}
{"error":"TOKEN=private-value build failed"}`
	var lines []string
	err := consumeDockerStream(strings.NewReader(input), func(line string) { lines = append(lines, line) })
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatal("Docker failure must propagate without credentials")
	}
	if len(lines) != 3 || !strings.Contains(lines[2], "Compiling") {
		t.Fatalf("missing build progress: %v", lines)
	}
}

func TestStageReportingPreservesFailureAndInvokesOperationOnce(t *testing.T) {
	want := errors.New("test failure")
	calls := 0
	err := reportStage(context.Background(), "test-group", "source-download", func() error { calls++; return want })
	if !errors.Is(err, want) || calls != 1 {
		t.Fatal("reporting must not retry or replace operation failures")
	}
}
