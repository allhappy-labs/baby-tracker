package auth

import (
	"net/http"
	"strings"
)

// BearerToken accepts an access credential only from one Authorization header.
// Query parameters, cookies, and WebSocket subprotocols are deliberately never
// treated as credential transports because those values are commonly logged.
func BearerToken(request *http.Request) (string, error) {
	if request == nil || request.URL == nil {
		return "", ErrInvalidRequest
	}
	for key := range request.URL.Query() {
		lower := strings.ToLower(key)
		if lower == "access_token" || lower == "bearer" || lower == "token" || lower == "authorization" {
			return "", ErrInvalidRequest
		}
	}
	for _, cookie := range request.Cookies() {
		lower := strings.ToLower(cookie.Name)
		if lower == "access_token" || lower == "bearer" || lower == "token" || lower == "authorization" {
			return "", ErrInvalidRequest
		}
	}
	for _, protocol := range request.Header.Values("Sec-WebSocket-Protocol") {
		for _, candidate := range strings.Split(protocol, ",") {
			candidate = strings.TrimSpace(strings.ToLower(candidate))
			if strings.HasPrefix(candidate, "bearer") || strings.HasPrefix(candidate, "token") {
				return "", ErrInvalidRequest
			}
		}
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", ErrInvalidRequest
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || len(token) > 4096 || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\t ") {
		return "", ErrInvalidRequest
	}
	return token, nil
}
