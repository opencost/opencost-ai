package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/opencost/opencost-ai/internal/metrics"
)

// metricsMiddleware is the per-endpoint wrapper that keeps
// requests_total, request_duration_seconds, and requests_in_flight
// in sync with every served request.
//
// The wrapper sees the endpoint label at construction time rather
// than deriving it at runtime, because r.URL.Path contains raw
// caller input and would blow up label cardinality if it held path
// parameters.
func metricsMiddleware(reg *metrics.Registry) func(endpoint string, next http.Handler) http.Handler {
	return func(endpoint string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reg.InFlight().Inc()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				reg.InFlight().Dec()
				reg.RequestDuration().WithLabelValues(endpoint, r.Method).
					Observe(time.Since(start).Seconds())
				reg.Requests().WithLabelValues(endpoint, r.Method, strconv.Itoa(sw.status)).Inc()
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// statusWriter captures the status code so the metrics middleware
// can label requests_total by the actual response status. It also
// passes through http.Flusher so the streaming SSE handler's flushes
// are not silently dropped.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// Match net/http's implicit-200 behaviour so handlers that
		// write without a prior WriteHeader still record a status.
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Flush proxies to the underlying ResponseWriter when it implements
// http.Flusher. The streaming SSE handler needs this so events are
// delivered to clients as they are produced rather than buffered
// until the response closes.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
