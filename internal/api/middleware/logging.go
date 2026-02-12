package middleware

import (
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Logger returns a middleware that logs HTTP requests
func Logger(logger *zap.Logger) func(next http.Handler) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code
			wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start)

			// Skip noisy health/ready probe logging
			path := r.URL.Path
			if path == "/api/v1/health" || path == "/api/v1/ready" {
				logger.Debug("HTTP request",
					zap.String("method", r.Method),
					zap.String("path", path),
					zap.Int("status", wrapped.statusCode),
					zap.Duration("duration", duration),
				)
				return
			}

			// Log the request
			logger.Info("HTTP request",
				zap.String("method", r.Method),
				zap.String("path", path),
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("user_agent", r.UserAgent()),
				zap.Int("status", wrapped.statusCode),
				zap.Duration("duration", duration),
			)
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
