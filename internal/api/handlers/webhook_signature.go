package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"
)

// signaturePrefix là tiền tố GitHub luôn gắn trước digest trong header
// X-Hub-Signature-256, ví dụ: "sha256=9f86d081...".
const signaturePrefix = "sha256="

// signatureHeader là tên header GitHub dùng để ký payload webhook bằng
// HMAC-SHA256.
const signatureHeader = "X-Hub-Signature-256"

// VerifyGitHubSignature là middleware bọc quanh webhook handler. Nó đọc
// toàn bộ body, tính HMAC-SHA256(body, secret) rồi so sánh với header
// X-Hub-Signature-256 mà GitHub gửi kèm. Nếu không khớp => 401 và KHÔNG
// gọi next (payload bị bỏ qua hoàn toàn).
//
// secret rỗng => bỏ qua xác thực (chỉ nên dùng khi chạy local/dev); ghi
// log cảnh báo để không vô tình deploy production ở trạng thái này.
func VerifyGitHubSignature(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			log.Printf("[webhook-signature] CẢNH BÁO: GITHUB_WEBHOOK_SECRET rỗng, bỏ qua xác thực chữ ký")
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		if !validSignature(secret, body, r.Header.Get(signatureHeader)) {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}

		// Trả lại body cho handler phía sau đọc tiếp.
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// validSignature tính digest kỳ vọng và so sánh theo kiểu hằng-thời-gian
// (hmac.Equal) để tránh timing attack.
func validSignature(secret string, body []byte, header string) bool {
	if !strings.HasPrefix(header, signaturePrefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	return hmac.Equal(got, want)
}
