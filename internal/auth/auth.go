package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// tokenPayload is the (unencrypted, signed) claim set carried by an API token.
// It is not secret — the signature is what authenticates it. iat is informational.
type tokenPayload struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
}

var b64 = base64.RawURLEncoding

// sign returns the base64url HMAC-SHA256 of msg under masterKey.
func sign(masterKey, msg string) string {
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte(msg))
	return b64.EncodeToString(mac.Sum(nil))
}

// MintToken issues a compact signed token of the form
// "<b64url(payloadJSON)>.<b64url(HMAC-SHA256(masterKey, b64url(payloadJSON)))>".
// The payload is not secret; the signature authenticates it. iat is the issue
// time (Unix seconds) — pass 0 to omit a meaningful timestamp.
func MintToken(masterKey string, iat int64) (string, error) {
	raw, err := json.Marshal(tokenPayload{Sub: "varhub", Iat: iat})
	if err != nil {
		return "", err
	}
	body := b64.EncodeToString(raw)
	return body + "." + sign(masterKey, body), nil
}

// VerifyToken reports whether tok is a well-formed token correctly signed by
// masterKey. The comparison is constant-time. There is no expiry check (tokens
// do not expire in this version).
func VerifyToken(masterKey, tok string) bool {
	body, sig, ok := strings.Cut(tok, ".")
	if !ok || body == "" || sig == "" {
		return false
	}
	if _, err := b64.DecodeString(body); err != nil {
		return false // body must be valid base64url
	}
	return hmac.Equal([]byte(sig), []byte(sign(masterKey, body)))
}

// Bearer extracts the token from an "Authorization: Bearer <token>" header,
// reporting whether the header was present and correctly formed.
func Bearer(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// Authed reports whether the request carries a valid bearer token. Callers use
// this to decide whether a caller is an admin (unscoped) or an anonymous session.
func Authed(masterKey string, r *http.Request) bool {
	tok, ok := Bearer(r)
	return ok && VerifyToken(masterKey, tok)
}

// RequireToken wraps h so it is reached only with a valid "Authorization: Bearer
// <token>" header signed by masterKey; otherwise it responds 401.
func RequireToken(masterKey string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Authed(masterKey, r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "missing or invalid bearer token",
			})
			return
		}
		h.ServeHTTP(w, r)
	})
}
