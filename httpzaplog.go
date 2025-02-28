package httpzaplog

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/slices"
)

type Options struct {
	Logger *zap.Logger

	// Concise determines Whether to log the entries in concise mode.
	Concise bool

	// SkipURLParams determines which get parameters shouldn't be logged.
	SkipURLParams []string

	// SkipHeaders determines which headers shouldn't be logged.
	SkipHeaders []string

	// SkipPaths determines which paths shouldn't be logged.
	SkipPaths []string

	// ErrorMiddleware is a middleware that will be injected between the logger middleware and the Recoverer middleware.
	// This allows you to customize the 500 error page in the case of a panic.
	ErrorMiddleware func(http.Handler) http.Handler
}

var DefaultOptions = Options{
	Logger:  zap.Must(zap.NewProduction()),
	Concise: false,
}

// RequestLogger is an http middleware to log http requests and responses.
//
// NOTE: for simplicity, RequestLogger automatically makes use of the chi RequestID and
// Recoverer middleware.
func RequestLogger(opts *Options) func(next http.Handler) http.Handler {
	if opts == nil {
		opts = &DefaultOptions
	}
	chain := []func(http.Handler) http.Handler{
		middleware.RequestID,
		Handler(opts),
	}
	if opts.ErrorMiddleware != nil {
		chain = append(chain, opts.ErrorMiddleware)
	}
	chain = append(chain, middleware.Recoverer)
	return chi.Chain(chain...).Handler
}

func Handler(opts *Options) func(next http.Handler) http.Handler {
	var f middleware.LogFormatter = &requestLogger{opts}

	skipPaths := map[string]struct{}{}
	for _, path := range opts.SkipPaths {
		skipPaths[path] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			// Skip the logger if the path is in the skip list
			if len(skipPaths) > 0 {
				_, skip := skipPaths[r.URL.Path]
				if skip {
					next.ServeHTTP(w, r)
					return
				}
			}

			// Log the request
			entry := f.NewLogEntry(r)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			buf := newLimitBuffer(512)
			ww.Tee(buf)

			t1 := time.Now()
			defer func() {
				var respBody []byte
				if ww.Status() >= 400 {
					respBody, _ = ioutil.ReadAll(buf)
				}
				entry.Write(ww.Status(), ww.BytesWritten(), ww.Header(), time.Since(t1), respBody)
			}()

			next.ServeHTTP(ww, middleware.WithLogEntry(r, entry))
		}
		return http.HandlerFunc(fn)
	}
}

type requestLogger struct {
	Opts *Options
}

func (l *requestLogger) NewLogEntry(r *http.Request) middleware.LogEntry {
	entry := &RequestLoggerEntry{
		concise:       l.Opts.Concise,
		skipURLParams: l.Opts.SkipURLParams,
		skipHeaders:   l.Opts.SkipHeaders,
	}
	msg := fmt.Sprintf("Request: %s %s", r.Method, r.URL.Path)

	entry.Logger = l.Opts.Logger.With(l.requestLogFields(r))
	if !l.Opts.Concise {
		entry.Logger.Info(msg)
	}
	return entry
}

type RequestLoggerEntry struct {
	Logger        *zap.Logger
	msg           string
	concise       bool
	skipURLParams []string
	skipHeaders   []string
}

func (l *RequestLoggerEntry) Write(status, bytes int, header http.Header, elapsed time.Duration, extra interface{}) {
	msg := fmt.Sprintf("Response: %d %s", status, statusLabel(status))
	if l.msg != "" {
		msg = fmt.Sprintf("%s - %s", msg, l.msg)
	}

	responseLog := map[string]interface{}{
		"status":  status,
		"bytes":   bytes,
		"elapsed": float64(elapsed.Nanoseconds()) / 1000000.0, // in milliseconds
	}

	if !l.concise {
		// Include response header, as well for error status codes (>400) we include
		// the response body so we may inspect the log message sent back to the client.
		if status >= 400 {
			body, _ := extra.([]byte)
			responseLog["body"] = string(body)
		}
		if len(header) > 0 {
			responseLog["header"] = headerLogField(header, l.skipHeaders, l.skipURLParams)
		}
	}

	l.Logger.With(zap.Any("httpResponse", responseLog)).
		Log(statusLevel(status), msg)
}

func (l *RequestLoggerEntry) Panic(v interface{}, stack []byte) {
	stacktrace := string(stack)

	l.Logger = l.Logger.With(
		zap.String("stacktrace", stacktrace),
		zap.String("panic", fmt.Sprintf("%+v", v)),
	)
	l.msg = fmt.Sprintf("%+v", v)
	middleware.PrintPrettyStack(v)
}

func (l *requestLogger) requestLogFields(r *http.Request) zapcore.Field {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	// Make sure to sanitize the get parameters in the request URL.
	var requestURL string
	parsed, err := url.Parse(r.RequestURI)
	if err != nil {
		requestURL = fmt.Sprintf("%s://%s%s", scheme, r.Host, r.URL.Path)
	} else {
		urlValues := parsed.Query()
		for urlK := range urlValues {
			if slices.Contains(l.Opts.SkipURLParams, urlK) {
				urlValues.Set(urlK, "***")
			}
		}
		parsed.RawQuery = urlValues.Encode()
		requestURL = fmt.Sprintf("%s://%s%s", scheme, r.Host, parsed.String())
	}

	requestFields := map[string]interface{}{
		"requestURL":    requestURL,
		"requestMethod": r.Method,
		"requestPath":   r.URL.Path,
		"remoteIP":      r.RemoteAddr,
		"proto":         r.Proto,
	}
	if reqID := middleware.GetReqID(r.Context()); reqID != "" {
		requestFields["requestID"] = reqID
	}

	if l.Opts.Concise {
		return zap.Any("httpRequest", requestFields)
	}

	requestFields["scheme"] = scheme

	if len(r.Header) > 0 {
		requestFields["header"] = headerLogField(r.Header, l.Opts.SkipHeaders, l.Opts.SkipURLParams)
	}

	return zap.Any("httpRequest", requestFields)
}

func headerLogField(header http.Header, skipHeaders []string, skipURLParams []string) map[string]string {
	headerField := map[string]string{}
	for k, v := range header {
		k = strings.ToLower(k)
		switch {
		case len(v) == 0:
			continue
		case len(v) == 1:
			headerField[k] = v[0]
		default:
			headerField[k] = fmt.Sprintf("[%s]", strings.Join(v, "], ["))
		}
		if k == "authorization" || k == "cookie" || k == "set-cookie" {
			headerField[k] = "***"
		}
		if k == "location" {
			// For location key, try to sanitize the URL parameters which may contain sensitive info, expecially with OIDC
			parsed, err := url.Parse(headerField[k])
			if err != nil {
				headerField[k] = "***"
			} else {
				urlValues := parsed.Query()
				for urlK := range urlValues {
					if slices.Contains(skipURLParams, urlK) {
						urlValues.Set(urlK, "***")
					}
				}
				parsed.RawQuery = urlValues.Encode()
				headerField[k] = parsed.String()
			}
		}

		if slices.Contains(skipHeaders, k) {
			headerField[k] = "***"
		}
	}
	return headerField
}

func statusLevel(status int) zapcore.Level {
	switch {
	case status <= 0:
		return zapcore.WarnLevel
	case status < 400: // for codes in 100s, 200s, 300s
		return zapcore.InfoLevel
	case status >= 400 && status < 500:
		return zapcore.WarnLevel
	case status >= 500:
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

func statusLabel(status int) string {
	switch {
	case status >= 100 && status < 300:
		return "OK"
	case status >= 300 && status < 400:
		return "Redirect"
	case status >= 400 && status < 500:
		return "Client Error"
	case status >= 500:
		return "Server Error"
	default:
		return "Unknown"
	}
}

// Helper methods used by the application to get the request-scoped
// logger entry and set additional fields between handlers.
//
// This is a useful pattern to use to set state on the entry as it
// passes through the handler chain, which at any point can be logged
// with a call to .Print(), .Info(), etc.

func LogEntry(ctx context.Context) *zap.Logger {
	entry, ok := ctx.Value(middleware.LogEntryCtxKey).(*RequestLoggerEntry)
	if !ok || entry == nil {
		return zap.NewNop()
	} else {
		return entry.Logger
	}
}

func LogEntrySetField(ctx context.Context, key, value string) {
	if entry, ok := ctx.Value(middleware.LogEntryCtxKey).(*RequestLoggerEntry); ok {
		entry.Logger = entry.Logger.With(zap.String(key, value))
	}
}

func LogEntrySetFields(ctx context.Context, fields ...zapcore.Field) {
	if entry, ok := ctx.Value(middleware.LogEntryCtxKey).(*RequestLoggerEntry); ok {
		entry.Logger = entry.Logger.With(fields...)
	}
}
