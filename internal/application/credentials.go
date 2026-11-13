package application

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
)

// PreviewSecret derives purpose-separated, preview-only credentials. These are
// injected at runtime, never into a Docker build or GitHub comment.
func PreviewSecret(secret, envID, purpose string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte("preview:" + purpose + ":" + envID))
	return hex.EncodeToString(mac.Sum(nil))
}
