package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type trackedResponseWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *trackedResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.hijacked = status == http.StatusSwitchingProtocols
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *trackedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func withMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := validOrNewRequestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", requestID)
		request := r.WithContext(contextWithLogger(contextWithRequestID(r.Context(), requestID), logger))
		bodyLimit := maxRequestBodyBytes
		if request.URL.Path == "/v1/sync" {
			bodyLimit = 4 << 20
		}
		request.Body = http.MaxBytesReader(w, request.Body, bodyLimit)
		tracked := &trackedResponseWriter{ResponseWriter: w}
		started := time.Now()

		defer func() {
			if recovered := recover(); recovered != nil {
				// The value is named by type only: a panic message can carry
				// anything the handler was holding.
				logger.LogAttrs(context.WithoutCancel(request.Context()), slog.LevelError, "request panicked",
					slog.String("request_id", requestID), slog.String("route", normalizedRoute(request.Pattern)),
					slog.String("panic_type", fmt.Sprintf("%T", recovered)))
				if !tracked.hijacked {
					writeError(tracked, http.StatusInternalServerError, "internal_error", "Internal server error", requestID, true)
				}
			}
			status := tracked.status
			if status == 0 {
				status = http.StatusOK
			}
			route := normalizedRoute(request.Pattern)
			logger.Info("request completed",
				slog.String("request_id", requestID),
				slog.String("method", request.Method),
				slog.String("route", route),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(started)),
			)
		}()

		next.ServeHTTP(tracked, request)
	})
}

func normalizedRoute(pattern string) string {
	if _, route, found := strings.Cut(pattern, " "); found {
		return route
	}
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}

type requestIDContextKey struct{}

func contextWithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

type requestLoggerContextKey struct{}

// contextWithLogger lets writers that have no Options reach the handler's
// logger: the middleware puts it on every request.
func contextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, requestLoggerContextKey{}, logger)
}

// requestLogger prefers the handler's own logger, then the one on the
// request; nil for a writer called outside both.
func requestLogger(r *http.Request, preferred *slog.Logger) *slog.Logger {
	if preferred != nil {
		return preferred
	}
	logger, _ := r.Context().Value(requestLoggerContextKey{}).(*slog.Logger)
	return logger
}

// logServerFault leaves one line per 5xx a shared writer sends: the route,
// the code the client saw and the cause that was folded into it (every layer
// collapses infrastructure failures to `service_unavailable`, so without this
// a database error and a lost connection look the same). A request whose
// client has already gone — the connection closed, the deadline passed — is
// not a server fault: its response goes nowhere, so the line says that at
// INFO instead, and a burst of abandoned requests cannot read as an outage.
func logServerFault(logger *slog.Logger, r *http.Request, status int, code string, cause error) {
	if logger == nil || status < http.StatusInternalServerError {
		return
	}
	clientGone := r.Context().Err() != nil
	attrs := []slog.Attr{
		slog.String("request_id", requestIDFromContext(r.Context())),
		slog.String("route", normalizedRoute(r.Pattern)),
		slog.Int("status", status),
		slog.String("code", code),
		slog.Bool("client_gone", clientGone),
	}
	if cause != nil {
		attrs = append(attrs, slog.String("cause", describeCause(cause)))
	}
	level, message := slog.LevelError, "request failed"
	if clientGone {
		level, message = slog.LevelInfo, "request abandoned by client"
	}
	logger.LogAttrs(context.WithoutCancel(r.Context()), level, message, attrs...)
}

const maxCauseBytes = 200

// codeShape is the shape of this server's own error codes: lowercase words
// joined by underscores, no digits. Nothing a provider or a client sends
// looks like that, so such text is safe to repeat.
var codeShape = regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)

// describeCause names an error chain for a log line without repeating text
// of unknown origin: provider and client text may carry secrets. Postgres
// errors, context, I/O and network failures and this server's own codes are
// spelled out; anything else appears by type. Bounded.
func describeCause(err error) string {
	var parts []string
	var walk func(error, int)
	walk = func(e error, depth int) {
		if e == nil || depth > 8 || len(parts) >= 8 {
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range u.Unwrap() {
				walk(child, depth+1)
			}
			return
		case interface{ Unwrap() error }:
			if child := u.Unwrap(); child != nil {
				walk(child, depth+1)
				return
			}
		}
		parts = append(parts, describeLeaf(e))
	}
	walk(err, 0)
	described := strings.Join(parts, "; ")
	if len(described) > maxCauseBytes {
		described = described[:maxCauseBytes]
	}
	return described
}

func describeLeaf(e error) string {
	var pgErr *pgconn.PgError
	var connect *pgconn.ConnectError
	var network net.Error
	switch {
	case errors.As(e, &pgErr):
		// A data exception (class 22) quotes the offending value, which may
		// be the client's own input: the code alone.
		if strings.HasPrefix(pgErr.Code, "22") {
			return "sqlstate " + pgErr.Code
		}
		return "sqlstate " + pgErr.Code + " " + pgErr.Message
	case errors.As(e, &connect):
		return "pgconn connect failed"
	case errors.Is(e, context.Canceled), errors.Is(e, context.DeadlineExceeded), errors.Is(e, io.EOF), errors.Is(e, io.ErrUnexpectedEOF), errors.Is(e, pgx.ErrNoRows):
		return e.Error()
	case errors.As(e, &network):
		if op, ok := e.(*net.OpError); ok {
			return fmt.Sprintf("%T %s", e, op.Op)
		}
		return fmt.Sprintf("%T", e)
	case codeShape.MatchString(e.Error()):
		return e.Error()
	}
	return fmt.Sprintf("%T", e)
}

func validOrNewRequestID(inbound string) string {
	if requestIDPattern.MatchString(inbound) {
		return inbound
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic("unable to generate request ID")
	}
	return hex.EncodeToString(bytes)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string, retryable bool) {
	writeJSON(w, status, newErrorResponse(code, message, requestID, retryable))
}
