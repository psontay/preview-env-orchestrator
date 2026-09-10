// Package github chứa adapter THẬT gọi GitHub REST API, implement
// application.GitHubNotifier. Bản giả lập (in ra console) vẫn nằm ở
// internal/infrastructure/memory và được dùng làm test double.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultAPIBaseURL là gốc GitHub REST API công khai. Tách ra biến để
// test có thể trỏ sang httptest.Server.
const defaultAPIBaseURL = "https://api.github.com"

// Notifier post comment ngược lại vào Pull Request qua endpoint
// POST /repos/{owner}/{repo}/issues/{number}/comments.
//
// Lưu ý: comment của PR dùng chung API với "issue comment" — GitHub coi
// mỗi PR cũng là một issue, nên đường dẫn là /issues/{number}/comments
// chứ không phải /pulls/...
type Notifier struct {
	token      string
	apiBaseURL string
	httpClient *http.Client
}

// NewNotifier tạo adapter với token cho trước. apiBaseURL rỗng => dùng
// gốc GitHub công khai; httpClient nil => dùng client mặc định có timeout.
func NewNotifier(token, apiBaseURL string, httpClient *http.Client) *Notifier {
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Notifier{
		token:      token,
		apiBaseURL: strings.TrimRight(apiBaseURL, "/"),
		httpClient: httpClient,
	}
}

// NewNotifierFromEnv đọc GITHUB_TOKEN (bắt buộc) và GITHUB_API_URL (tuỳ
// chọn, cho GitHub Enterprise) từ biến môi trường.
func NewNotifierFromEnv() (*Notifier, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("github: thiếu biến môi trường GITHUB_TOKEN")
	}
	return NewNotifier(token, os.Getenv("GITHUB_API_URL"), nil), nil
}

// commentRequest là body JSON mà GitHub yêu cầu cho endpoint tạo comment.
type commentRequest struct {
	Body string `json:"body"`
}

// CommentOnPR implement application.GitHubNotifier. repo có dạng
// "owner/repo" (chính là field full_name trong payload webhook).
func (n *Notifier) CommentOnPR(ctx context.Context, repo string, prNumber int, message string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("github: repo không hợp lệ %q (cần dạng owner/repo)", repo)
	}

	body, err := json.Marshal(commentRequest{Body: message})
	if err != nil {
		return fmt.Errorf("github: marshal body: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", n.apiBaseURL, owner, name, prNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github: tạo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: gọi API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github: API trả về %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
