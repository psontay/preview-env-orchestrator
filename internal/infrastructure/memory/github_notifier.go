package memory

import (
	"context"
	"fmt"
)

// GitHubNotifier là bản giả lập của application.GitHubNotifier — in ra
// console thay vì gọi GitHub API thật. Bản thật xem docs/GITHUB_API_INTEGRATION.md.
type GitHubNotifier struct{}

func NewGitHubNotifier() *GitHubNotifier {
	return &GitHubNotifier{}
}

func (n *GitHubNotifier) CommentOnPR(_ context.Context, repo string, prNumber int, message string) error {
	fmt.Printf("[github-notifier] %s#%d: %s\n", repo, prNumber, message)
	return nil
}
