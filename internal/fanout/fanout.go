// Package fanout executes several upstream requests in parallel with all-or-nothing semantics and a cap on
// the total buffered response size (PLAN §9). Merging the responses is done elsewhere.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// ErrMergeTooLarge is returned when the buffered responses exceed the byte cap (502 merge_too_large).
var ErrMergeTooLarge = errors.New("merge_too_large")

// maxErrorSnippet bounds how much of an upstream error body is surfaced to the client.
const maxErrorSnippet = 256

// UpstreamError is a failed branch: a transport error or a non-2xx response.
type UpstreamError struct {
	// Index is the position of the failed request in the input slice.
	Index int
	// Status is the upstream status code, 0 for transport errors.
	Status int
	// Detail is a short, sanitised description (status and truncated body, or the transport error).
	Detail string
	Err    error
}

// Error implements error.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream request %d failed: %s", e.Index, e.Detail)
}

// Unwrap returns the transport error, if any.
func (e *UpstreamError) Unwrap() error { return e.Err }

// Doer sends HTTP requests; *http.Client implements it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Response is one fully buffered upstream response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Options bound a Run.
type Options struct {
	// MaxParallel limits concurrent requests (values < 1 mean 1).
	MaxParallel int
	// MaxBytes caps the total of all response bodies (values < 1 mean no body is allowed).
	MaxBytes int64
}

// Run sends all requests and returns their responses in input order. Any failure (transport error, non-2xx
// status, byte cap exceeded) cancels the remaining requests and fails the whole call. The request contexts
// are replaced by one derived from ctx. Every request is sent with Accept: application/json.
func Run(ctx context.Context, d Doer, reqs []*http.Request, opt Options) ([]Response, error) {
	if opt.MaxParallel < 1 {
		opt.MaxParallel = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		out      = make([]Response, len(reqs))
		sem      = make(chan struct{}, opt.MaxParallel)
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		used     atomic.Int64
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for i, r := range reqs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, r *http.Request) {
			defer wg.Done()
			defer func() { <-sem }()
			resp, err := one(ctx, d, r, i, opt.MaxBytes, &used)
			if err != nil {
				fail(err)
				return
			}
			out[i] = resp
		}(i, r)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		// The parent context ended (client gone or timeout) before every request started.
		return nil, &UpstreamError{Index: -1, Detail: err.Error(), Err: err}
	}
	return out, nil
}

func one(ctx context.Context, d Doer, r *http.Request, idx int, maxBytes int64, used *atomic.Int64) (Response, error) {
	r = r.Clone(ctx)
	r.Header.Set("Accept", "application/json")
	resp, err := d.Do(r)
	if err != nil {
		return Response{}, &UpstreamError{Index: idx, Detail: sanitize(err), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		return Response{}, &UpstreamError{Index: idx, Status: resp.StatusCode, Detail: fmt.Sprintf("status %d: %s", resp.StatusCode, snippet)}
	}

	// Read at most the remaining budget plus one byte, so exceeding it is detected without buffering more.
	remaining := maxBytes - used.Load()
	if remaining < 0 {
		remaining = 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, remaining+1))
	if err != nil {
		return Response{}, &UpstreamError{Index: idx, Detail: sanitize(err), Err: err}
	}
	if used.Add(int64(len(body))) > maxBytes {
		return Response{}, ErrMergeTooLarge
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// sanitize drops the request URL that net/http embeds in transport errors.
func sanitize(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	return err.Error()
}
