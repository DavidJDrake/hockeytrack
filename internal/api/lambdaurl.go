package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
)

// LambdaURLHandler adapts an http.Handler to a Lambda Function URL (payload
// format 2.0, the same shape API Gateway HTTP APIs use). Written here
// rather than taken as a dependency: it is a page of code, and it keeps
// the API runnable as a plain local server.
func LambdaURLHandler(h http.Handler) func(context.Context, events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	return func(ctx context.Context, ev events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		req, err := toRequest(ctx, ev)
		if err != nil {
			return events.LambdaFunctionURLResponse{StatusCode: http.StatusBadRequest, Body: `{"error":{"code":"bad_request","message":"malformed request"}}`,
				Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}}, nil
		}
		rec := &recorder{header: http.Header{}, status: http.StatusOK}
		h.ServeHTTP(rec, req)
		return rec.response(), nil
	}
}

func toRequest(ctx context.Context, ev events.LambdaFunctionURLRequest) (*http.Request, error) {
	method := ev.RequestContext.HTTP.Method
	if method == "" {
		method = http.MethodGet
	}
	u := &url.URL{Path: ev.RawPath, RawQuery: ev.RawQueryString}
	if u.Path == "" {
		u.Path = "/"
	}
	var body []byte
	if ev.Body != "" {
		if ev.IsBase64Encoded {
			b, err := base64.StdEncoding.DecodeString(ev.Body)
			if err != nil {
				return nil, err
			}
			body = b
		} else {
			body = []byte(ev.Body)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range ev.Headers {
		req.Header.Set(k, v)
	}
	if host := ev.Headers["host"]; host != "" {
		req.Host = host
	}
	if ip := ev.RequestContext.HTTP.SourceIP; ip != "" {
		req.RemoteAddr = ip
	}
	return req, nil
}

// recorder captures a handler's response for the Function URL reply.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

func (r *recorder) response() events.LambdaFunctionURLResponse {
	resp := events.LambdaFunctionURLResponse{StatusCode: r.status, Headers: map[string]string{}}
	for k, vs := range r.header {
		if len(vs) > 0 {
			resp.Headers[k] = strings.Join(vs, ", ")
		}
	}
	// JSON is text and goes through as is; anything else is base64 so a
	// stray byte can never corrupt the envelope.
	if utf8.Valid(r.body.Bytes()) {
		resp.Body = r.body.String()
	} else {
		resp.Body = base64.StdEncoding.EncodeToString(r.body.Bytes())
		resp.IsBase64Encoded = true
	}
	return resp
}
