// +build !go1.5

package sfxclient

import (
	"golang.org/x/net/context"
	"net/http"
)

func (h *HTTPDatapointSink) withCancel(ctx context.Context, req *http.Request) (err error) {
	type canceler interface {
		CancelRequest(*http.Request)
	}
	canCancel, ok := h.Client.Transport.(canceler)
	if !ok {
		return h.handleResponse(h.Client.Do(req))
	}

	c := make(chan error, 1)
	go func() { c <- h.handleResponse(h.Client.Do(req)) }()
	select {
	case <-ctx.Done():
		canCancel.CancelRequest(req)
		<-c // Wait for f to return.
		return ctx.Err()
	case err := <-c:
		return err
	}
}
