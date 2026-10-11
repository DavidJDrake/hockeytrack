package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cache lifetimes, sent as Cache-Control so CloudFront and any client cache
// the same way. A finished game never changes; a live one changes every
// poll; the index and schedule change a few times a day.
const (
	ttlFinal     = 24 * time.Hour
	ttlLive      = 10 * time.Second
	ttlIndex     = 5 * time.Minute
	ttlStandings = time.Hour
	ttlStatic    = 24 * time.Hour
)

// apiError is a response the client can act on: a status and a short code.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func badRequest(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusNotFound, Code: "not_found", Message: fmt.Sprintf(format, args...)}
}

func badGateway(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadGateway, Code: "bad_feed", Message: fmt.Sprintf(format, args...)}
}

// errNotFound marks a loader miss so the cache can remember it briefly.
var errNotFound = errors.New("not found")

// handler is an endpoint that returns its body and cache lifetime, or an
// error. The wrapper turns all three into HTTP.
type handler func(r *http.Request) (body any, ttl time.Duration, err error)

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ttl, err := h(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, ttl, body)
	}
}

func writeJSON(w http.ResponseWriter, status int, ttl time.Duration, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Access-Control-Allow-Origin", "*")
	if ttl > 0 {
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(ttl/time.Second)))
	} else {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(body); err != nil {
		slog.Warn("response write failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
	case errors.Is(err, errNotFound):
		ae = notFound("%s", strings.TrimSuffix(err.Error(), ": "+errNotFound.Error()))
	case r.Context().Err() != nil:
		ae = &apiError{Status: 499, Code: "canceled", Message: "request canceled"}
	default:
		slog.Error("request failed", "path", r.URL.Path, "query", r.URL.RawQuery, "err", err)
		ae = &apiError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "the archive could not be read; retry shortly"}
		w.Header().Set("Retry-After", "5")
	}
	// A client error is cached briefly at the edge so a crawler walking
	// unknown ids does not reach the function for every one; a server
	// error is never cached, so a recovered archive answers at once.
	ttl := time.Duration(0)
	if ae.Status >= 400 && ae.Status < 500 {
		ttl = time.Minute
	}
	writeJSON(w, ae.Status, ttl, map[string]any{"error": map[string]string{"code": ae.Code, "message": ae.Message}})
}

// page is the envelope every list endpoint returns.
type page struct {
	Season int64 `json:"season,omitempty"`
	Total  int   `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
	Items  any   `json:"items"`
}

const (
	defaultLimit = 100
	maxLimit     = 1000
)

// paging reads limit and offset, bounding both.
func paging(r *http.Request) (limit, offset int, err error) {
	limit, offset = defaultLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, 0, badRequest("limit must be a positive integer")
		}
		limit = min(n, maxLimit)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, badRequest("offset must be a non-negative integer")
		}
		offset = n
	}
	return limit, offset, nil
}

// slicePage applies limit and offset to a slice and wraps it.
func slicePage[T any](season int64, items []T, limit, offset int) page {
	total := len(items)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	out := items[offset:end]
	if out == nil {
		out = []T{}
	}
	return page{Season: season, Total: total, Limit: limit, Offset: offset, Items: out}
}

// queryInt reads an optional integer parameter.
func queryInt(r *http.Request, key string) (int, bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false, badRequest("%s must be an integer", key)
	}
	return n, true, nil
}

// queryInt64 reads an optional 64-bit integer parameter.
func queryInt64(r *http.Request, key string) (int64, bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, badRequest("%s must be an integer", key)
	}
	return n, true, nil
}

// queryDate reads an optional YYYY-MM-DD parameter; "today" is honored.
func queryDate(r *http.Request, key string, now time.Time) (string, bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return "", false, nil
	}
	if v == "today" {
		return now.UTC().Format("2006-01-02"), true, nil
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", false, badRequest("%s must be a date (YYYY-MM-DD)", key)
	}
	return v, true, nil
}

// queryTeam reads an optional team abbreviation, upper-cased. Abbreviations
// are two to four letters; anything else is refused before it can reach a
// lookup.
func queryTeam(r *http.Request, key string) (string, error) {
	return cleanTeam(r.URL.Query().Get(key))
}

func cleanTeam(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	v = strings.ToUpper(v)
	if len(v) < 2 || len(v) > 4 {
		return "", badRequest("team must be an abbreviation such as TBL")
	}
	for _, c := range v {
		if c < 'A' || c > 'Z' {
			return "", badRequest("team must be an abbreviation such as TBL")
		}
	}
	return v, nil
}

// pathID reads a numeric path value.
func pathID(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0, badRequest("%s must be a positive integer", name)
	}
	return n, nil
}

// querySeason reads season=, defaulting to the newest indexed season.
func (s *Server) querySeason(r *http.Request) (int64, error) {
	n, ok, err := queryInt64(r, "season")
	if err != nil {
		return 0, err
	}
	if ok {
		if n < 19171918 || n > 29992999 || n%10000 != n/10000+1 {
			return 0, badRequest("season must look like 20252026")
		}
		return n, nil
	}
	return s.currentSeason(r.Context())
}
