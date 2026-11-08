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

	"babytracker/server/internal/requestlog"
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
	return withRefusalLog(logger, newRefusalLog(logger, refusalLogInterval), next)
}

// withRefusalLog logs one "request completed" line per request, except for
// responses a handler marked with requestlog.Aggregate (repeat over-limit
// refusals): those are counted and summarized by refusals instead, so a
// refusal flood cannot crowd every other line out of the log.
func withRefusalLog(logger *slog.Logger, refusals *refusalLog, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := validOrNewRequestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", requestID)
		markedCtx, aggregated := requestlog.WithMark(contextWithRefusalFaults(contextWithLogger(contextWithRequestID(r.Context(), requestID), logger), refusals.faults))
		request := r.WithContext(markedCtx)
		bodyLimit := maxRequestBodyBytes
		if request.URL.Path == "/v1/sync" {
			bodyLimit = 4 << 20
		}
		request.Body = http.MaxBytesReader(w, request.Body, bodyLimit)
		tracked := &trackedResponseWriter{ResponseWriter: w}
		started := time.Now()

		defer func() {
			panicked := false
			if recovered := recover(); recovered != nil {
				// The value is named by type only: a panic message can carry
				// anything the handler was holding.
				logger.LogAttrs(context.WithoutCancel(request.Context()), slog.LevelError, "request panicked",
					slog.String("request_id", requestID), slog.String("route", normalizedRoute(request.Pattern)),
					slog.String("panic_type", fmt.Sprintf("%T", recovered)))
				if !tracked.hijacked {
					panicked = true
					writeError(tracked, http.StatusInternalServerError, "internal_error", "Internal server error", requestID, true)
				}
			}
			status := tracked.status
			if status == 0 {
				status = http.StatusOK
			}
			route := normalizedRoute(request.Pattern)
			if aggregated() && !panicked {
				refusals.add(route, status)
				return
			}
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
// client has already gone (cause_class client_gone) is not a server fault:
// its response goes nowhere, so the line says that at INFO instead, and a
// burst of abandoned requests cannot read as an outage. A request that ran
// out of the server's own time (cause_class timed_out) is a fault. A
// request that lost a race (lostRaceClass) is not one either: the line
// says so at WARN, with the same cause fields, and the client retries.
// Lost-race lines are spaced like refusal lines (refusalFaults): at most
// one a second per code, carrying the count left out since the last.
func logServerFault(logger *slog.Logger, r *http.Request, status int, code string, cause error) {
	if logger == nil || status < http.StatusInternalServerError {
		return
	}
	class := causeClass(r.Context(), cause)
	clientGone := class == causeClientGone
	race, lost := lostRaceClass(code)
	var suppressed int64
	if lost && !clientGone {
		// Outside the middleware there is no spacing: every line is kept.
		if faults, _ := r.Context().Value(refusalFaultsContextKey{}).(*refusalFaults); faults != nil {
			var admitted bool
			if suppressed, admitted = faults.admit(race); !admitted {
				return
			}
		}
	}
	attrs := []slog.Attr{
		slog.String("request_id", requestIDFromContext(r.Context())),
		slog.String("route", normalizedRoute(r.Pattern)),
		slog.Int("status", status),
		slog.String("code", code),
		slog.Bool("client_gone", clientGone),
	}
	if class != "" {
		attrs = append(attrs, slog.String("cause_class", class))
	}
	attrs = append(attrs, causeAttrs(cause)...)
	level, message := slog.LevelError, "request failed"
	switch {
	case clientGone:
		level, message = slog.LevelInfo, "request abandoned by client"
	case lost:
		level, message = slog.LevelWarn, "request lost a race"
		attrs = append(attrs, slog.Int64("suppressed", suppressed))
	}
	logger.LogAttrs(context.WithoutCancel(r.Context()), level, message, attrs...)
}

// lostRaceClass is code's spacing class when code is a retryable 503 for a
// request that lost a race with a concurrent one (an identical command, a
// twin registration, a sign-in whose account was deleted under it, a
// credential rotation in progress); every writer answers it retryable.
func lostRaceClass(code string) (refusalClass, bool) {
	switch code {
	case "security_busy":
		return refusalSecurityBusy, true
	case "credential_rotation_busy":
		return refusalCredentialRotationBusy, true
	}
	return 0, false
}

const (
	causeTimedOut   = "timed_out"
	causeClientGone = "client_gone"
)

// causeClass tells the server's own deadline from a client that left. Only
// net/http cancels a request's context when its client goes (context
// canceled); a deadline on the request, or one inside the handler while the
// request is still alive, is the server's own budget running out. Anything
// else has no class.
func causeClass(ctx context.Context, cause error) string {
	switch err := ctx.Err(); {
	case errors.Is(err, context.Canceled):
		return causeClientGone
	case errors.Is(err, context.DeadlineExceeded):
		return causeTimedOut
	case err == nil && errors.Is(cause, context.DeadlineExceeded):
		return causeTimedOut
	}
	return ""
}

// causeAttrs are a fault line's cause fields: cause (describeCause),
// cause_type (each leaf's Go type) and, when any leaf is one, cause_code
// (the leaves that are code-shaped sentinels). None for a nil cause.
func causeAttrs(cause error) []slog.Attr {
	if cause == nil {
		return nil
	}
	described := describeCauseFields(cause)
	attrs := []slog.Attr{slog.String("cause", described.text), slog.String("cause_type", described.types)}
	if described.codes != "" {
		attrs = append(attrs, slog.String("cause_code", described.codes))
	}
	return attrs
}

const maxCauseBytes = 200

// codeShape is the shape of this server's own error codes as cause spells
// them: lowercase words joined by underscores, no digits. Nothing a
// provider or a client sends looks like that, so such text is safe to
// repeat.
var codeShape = regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)

// causeCodeShape is the shape of a sentinel error's code (cause_code): a
// lowercase identifier of at most 64 bytes. Messages are trusted by shape,
// so no error may be built from client data: TestErrorsAreNeverBuiltFromValues
// refuses errors.New of a value and fmt.Errorf of a bare value.
var causeCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// codeShaped reports whether message is a code that is safe to repeat: the
// code shape, with no part that reads like a digest, token or ID (an
// underscore-separated part longer than 32 bytes, or longer than 8 with a
// digit in it). Nothing a provider or a client sends looks like that.
func codeShaped(message string) bool {
	if !causeCodeShape.MatchString(message) {
		return false
	}
	for _, part := range strings.Split(message, "_") {
		if len(part) > 32 || len(part) > 8 && strings.ContainsAny(part, "0123456789") {
			return false
		}
	}
	return true
}

// describeCause names an error chain for a log line without repeating text
// of unknown origin: provider and client text may carry secrets. Postgres
// errors, context, I/O and network failures and this server's own codes are
// spelled out; anything else appears by type. Bounded.
func describeCause(err error) string {
	return describeCauseFields(err).text
}

// causeDescription is an error chain's leaves described three ways, each
// joined with "; " and bounded: text (describeLeaf), types (each leaf's Go
// type) and codes (the code-shaped leaves' messages, "" when none is).
type causeDescription struct {
	text, types, codes string
}

func describeCauseFields(err error) causeDescription {
	var parts, types, codes []string
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
		types = append(types, fmt.Sprintf("%T", e))
		if message := e.Error(); codeShaped(message) {
			codes = append(codes, message)
		}
	}
	walk(err, 0)
	return causeDescription{text: boundedJoin(parts), types: boundedJoin(types), codes: boundedJoin(codes)}
}

func boundedJoin(parts []string) string {
	joined := strings.Join(parts, "; ")
	if len(joined) > maxCauseBytes {
		joined = joined[:maxCauseBytes]
	}
	return joined
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
	if direct, ok := value.(jsonAppender); ok {
		if body, err := direct.AppendJSON(nil); err == nil {
			w.WriteHeader(status)
			_, _ = w.Write(append(body, '\n'))
			return
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// jsonAppender is a value with a direct encoder (protocol.SyncResponse):
// AppendJSON appends exactly what json.NewEncoder(w).Encode(value) writes,
// without the trailing newline, and without compacting stored payloads
// again. On its error writeJSON encodes value as before (which writes
// nothing when that fails too).
type jsonAppender interface {
	AppendJSON([]byte) ([]byte, error)
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string, retryable bool) {
	writeJSON(w, status, newErrorResponse(code, message, requestID, retryable))
}
