package api

import (
	"net/http"
	"regexp"
	"time"

	idgen "receipt-autoitemize/internal/id"
	"receipt-autoitemize/internal/reqid"
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// middleware wraps every route. Order matters: the access log sits outside panic
// recovery, so a request that panics is still logged, with the 500 it got.
func (s *Server) middleware(h http.Handler) http.Handler {
	return s.withRequestID(s.withAccessLog(s.withRecover(h)))
}

// withRequestID propagates a caller's X-Request-ID or mints one, and echoes it back.
// reqid.Handler then adds it to every log line written with this request's context.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID.MatchString(id) {
			id = idgen.New("req")
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(reqid.With(r.Context(), id)))
	})
}

func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { // a sentinel panic value, compared by identity
				panic(p) // net/http's deliberate "abort this response"; not a bug
			}
			s.log.ErrorContext(r.Context(), "panic", "panic", p)
			if rec, ok := w.(*statusRecorder); ok && rec.wrote {
				return // headers are out; a JSON error body now would corrupt the response
			}
			s.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error", nil)
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer (flush, deadlines).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// Deferred so the line is written even when the handler aborts by panicking.
		defer func() {
			s.log.InfoContext(r.Context(), "request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}
