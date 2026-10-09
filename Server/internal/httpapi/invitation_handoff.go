package httpapi

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"
	"strings"
)

//go:embed assets/invitation.html
var invitationHandoffHTML string

var invitationHandoffCSP = handoffCSP(invitationHandoffHTML)

func invitationHandoffHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", invitationHandoffCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(invitationHandoffHTML))
}

func handoffCSP(document string) string {
	scriptHash := inlineBlockHash(document, "<script>", "</script>", "invitation handoff script is missing")
	styleHash := inlineBlockHash(document, "<style>", "</style>", "invitation handoff style is missing")
	return "default-src 'none'; script-src 'sha256-" + scriptHash + "'; style-src 'sha256-" + styleHash + "'; connect-src 'none'; img-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}

// inlineBlockHash returns the base64 sha256 of the text between the first
// opening and closing tag, the form a CSP hash source expects.
func inlineBlockHash(document, opening, closing, missing string) string {
	start := strings.Index(document, opening)
	end := strings.Index(document, closing)
	if start < 0 || end < start {
		panic(missing)
	}
	digest := sha256.Sum256([]byte(document[start+len(opening) : end]))
	return base64.StdEncoding.EncodeToString(digest[:])
}
