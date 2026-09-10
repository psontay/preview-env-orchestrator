package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotifier_CommentOnPR_Success(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	n := NewNotifier("tok-123", srv.URL, srv.Client())
	if err := n.CommentOnPR(context.Background(), "org/app", 42, "hello world"); err != nil {
		t.Fatalf("CommentOnPR lỗi: %v", err)
	}

	if want := "/repos/org/app/issues/42/comments"; gotPath != want {
		t.Errorf("path = %q, muốn %q", gotPath, want)
	}
	if want := "Bearer tok-123"; gotAuth != want {
		t.Errorf("Authorization = %q, muốn %q", gotAuth, want)
	}
	var payload commentRequest
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body không phải JSON hợp lệ: %v", err)
	}
	if payload.Body != "hello world" {
		t.Errorf("body.body = %q, muốn %q", payload.Body, "hello world")
	}
}

func TestNotifier_CommentOnPR_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	n := NewNotifier("bad", srv.URL, srv.Client())
	err := n.CommentOnPR(context.Background(), "org/app", 1, "x")
	if err == nil {
		t.Fatal("muốn lỗi khi API trả về 403, nhưng err == nil")
	}
}

func TestNotifier_CommentOnPR_InvalidRepo(t *testing.T) {
	n := NewNotifier("tok", "http://example.invalid", http.DefaultClient)
	if err := n.CommentOnPR(context.Background(), "no-slash", 1, "x"); err == nil {
		t.Fatal("muốn lỗi với repo không có dạng owner/repo")
	}
}

func TestNewNotifierFromEnv_MissingToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := NewNotifierFromEnv(); err == nil {
		t.Fatal("muốn lỗi khi thiếu GITHUB_TOKEN")
	}
}
