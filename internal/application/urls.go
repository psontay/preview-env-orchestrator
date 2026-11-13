package application

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	return CancelDeletionURL(baseURL, env.ID, "")
}

func CancelDeletionURL(baseURL, id, secret string) string {
	result := strings.TrimRight(baseURL, "/") + "/environments/" + url.PathEscape(id) + "/cancel-deletion"
	if secret != "" {
		result += "?token=" + cancelToken(id, secret)
	}
	return result
}

func cancelToken(id, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("cancel-deletion:" + id))
	return hex.EncodeToString(mac.Sum(nil))
}

func ValidCancelToken(id, token, secret string) bool {
	return secret != "" && hmac.Equal([]byte(token), []byte(cancelToken(id, secret)))
}
