package server

import (
	"encoding/json"
	"net/http"

	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// writeProblem emits an RFC 7807 problem+json document with the
// canonical Content-Type and the gateway's request ID threaded
// through both the Instance field and the top-level RequestID
// extension member. All non-2xx responses from the server package
// go through this helper so the wire shape stays consistent.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	reqID := requestIDFromContext(r.Context())
	prob := apiv1.Problem{
		Title:     title,
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.Path,
		RequestID: reqID,
	}
	body, err := json.Marshal(prob)
	if err != nil {
		// json.Marshal on a Problem cannot fail for any runtime
		// input — the types are all string/int. A failure here
		// indicates a build-time regression; degrade to plain text
		// rather than panic in the request path.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error\n"))
		return
	}
	w.Header().Set("Content-Type", apiv1.ProblemContentType)
	if reqID != "" {
		w.Header().Set("X-Request-ID", reqID)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
