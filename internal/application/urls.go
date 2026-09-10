package application

import (
	"net/url"
	"strings"

	"preview-env-orchestrator/internal/domain"
)

// cancelDeletionURL dựng URL công khai trỏ tới endpoint hủy lệnh xoá
// (POST /environments/{id}/cancel-deletion) để nhúng vào comment PR.
//
// env.ID được percent-encode vì idempotency key thật có dạng
// "owner/repo:pr_id:commit" (chứa dấu "/").
func cancelDeletionURL(baseURL string, env *domain.PreviewEnvironment) string {
	return strings.TrimRight(baseURL, "/") + "/environments/" + url.PathEscape(env.ID) + "/cancel-deletion"
}
