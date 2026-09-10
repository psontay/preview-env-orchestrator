package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "s3cr3t"

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// spyHandler ghi nhận việc nó có được gọi hay không và body nó đọc được.
type spyHandler struct {
	called  bool
	gotBody string
}

func (s *spyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	b, _ := io.ReadAll(r.Body)
	s.gotBody = string(b)
	w.WriteHeader(http.StatusOK)
}

func TestVerifyGitHubSignature_ValidSignature_PassesThrough(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	spy := &spyHandler{}
	mw := VerifyGitHubSignature(testSecret, spy)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(body)))
	req.Header.Set(signatureHeader, sign(testSecret, body))
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if !spy.called {
		t.Fatal("handler phía sau phải được gọi khi chữ ký hợp lệ")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, muốn 200", rec.Code)
	}
	if spy.gotBody != string(body) {
		t.Errorf("body handler đọc được = %q, muốn %q", spy.gotBody, string(body))
	}
}

func TestVerifyGitHubSignature_InvalidSignature_Rejected(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	spy := &spyHandler{}
	mw := VerifyGitHubSignature(testSecret, spy)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(body)))
	req.Header.Set(signatureHeader, sign("wrong-secret", body))
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if spy.called {
		t.Fatal("handler phía sau KHÔNG được gọi khi chữ ký sai")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, muốn 401", rec.Code)
	}
}

func TestVerifyGitHubSignature_MissingHeader_Rejected(t *testing.T) {
	spy := &spyHandler{}
	mw := VerifyGitHubSignature(testSecret, spy)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if spy.called {
		t.Fatal("thiếu header chữ ký thì phải bị từ chối")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, muốn 401", rec.Code)
	}
}

func TestVerifyGitHubSignature_EmptySecret_SkipsVerification(t *testing.T) {
	spy := &spyHandler{}
	mw := VerifyGitHubSignature("", spy)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if !spy.called {
		t.Fatal("secret rỗng => bỏ qua xác thực, handler vẫn phải chạy")
	}
}
