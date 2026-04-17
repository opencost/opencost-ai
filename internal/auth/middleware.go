package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// bearerScheme is the only auth scheme the gateway accepts in v0.1.
// Kept lower-case so the comparison below can fold case cheaply.
const bearerScheme = "bearer"

// Validator is the minimal interface Middleware needs. Source
// satisfies it; tests can supply a fake without touching the
// filesystem.
type Validator interface {
	Validate(token string) error
}

// Middleware returns an http.Handler that requires a valid bearer
// token on every request, delegating the token check to v. On
// failure it writes an RFC 7807 problem+json document and short-
// circuits the wrapped handler.
//
// logger is used only for warning-level events that are safe to log
// (missing header, wrong scheme, invalid token); the token value is
// never logged.
func Middleware(v Validator, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := extractBearer(r.Header.Get("Authorization"))
			if err != nil {
				logger.Warn("auth header rejected",
					"remote", r.RemoteAddr, "path", r.URL.Path, "err", err)
				writeProblem(w, r, http.StatusUnauthorized,
					"Unauthorized", err.Error())
				return
			}
			switch err := v.Validate(token); {
			case err == nil:
				next.ServeHTTP(w, r)
			case errors.Is(err, ErrNoToken):
				// Operator bug, not a caller bug — surface as 503 so
				// a load balancer will back off rather than fail
				// authenticated retries.
				logger.Error("auth: no token configured",
					"path", r.URL.Path, "err", err)
				writeProblem(w, r, http.StatusServiceUnavailable,
					"Service Unavailable",
					"authentication is not configured on this gateway")
			case errors.Is(err, ErrInvalidToken):
				logger.Warn("auth: invalid token",
					"remote", r.RemoteAddr, "path", r.URL.Path)
				writeProblem(w, r, http.StatusUnauthorized,
					"Unauthorized", "invalid bearer token")
			default:
				logger.Error("auth: validator error",
					"path", r.URL.Path, "err", err)
				writeProblem(w, r, http.StatusInternalServerError,
					"Internal Server Error",
					"authentication check failed")
			}
		})
	}
}

// extractBearer parses an RFC 6750 Authorization header value,
// returning the token portion. It rejects empty headers, the wrong
// scheme, and schemes with no token. Error messages are caller-safe
// by construction — they describe the violation, never the value.
func extractBearer(header string) (string, error) {
	if header == "" {
		return "", errors.New("missing Authorization header")
	}
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		return "", errors.New("malformed Authorization header")
	}
	if !strings.EqualFold(scheme, bearerScheme) {
		return "", errors.New("unsupported authorization scheme, expected Bearer")
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", errors.New("empty bearer token")
	}
	return token, nil
}

// writeProblem emits an RFC 7807 problem+json document. Duplicated
// in miniature from internal/server because the auth package runs
// ahead of the server's handler chain — the alternative is a
// circular import. The two helpers must stay consistent; see
// internal/server/problem.go.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	body, _ := json.Marshal(apiv1.Problem{
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
	})
	w.Header().Set("Content-Type", apiv1.ProblemContentType)
	// RFC 6750 §3: 401 responses should include WWW-Authenticate.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="opencost-ai"`)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
