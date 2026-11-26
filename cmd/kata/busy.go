package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/api"
)

func configureBusy(c *http.Client, budget time.Duration, stream bool) {
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = busyTransport{next: next, budget: budget, stream: stream}
}

type busyTransport struct {
	next   http.RoundTripper
	budget time.Duration
	stream bool
}

func (t busyTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t busyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var retry *BusyRetry
	var lastResponse *http.Response
	var lastMessage string
	var lastDelay time.Duration
	request := req
	for {
		resp, err := t.next.RoundTrip(request)
		if err != nil {
			if lastResponse != nil && (req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions) && errors.Is(err, context.DeadlineExceeded) {
				return t.expiredBusy(lastResponse, lastMessage, lastDelay)
			}
			return resp, err
		}
		message, code, busy := api.InspectBusyResponse(resp, req.URL.Hostname())
		if !busy {
			return resp, nil
		}
		if retry == nil {
			retry = NewBusyRetry(t.budget)
		}
		if code != api.CodeServerBusy {
			setBusyResponse(resp, message)
		}
		delay, retryable := retry.Next(resp.Header.Get("Retry-After"))
		lastResponse, lastMessage, lastDelay = resp, message, delay
		callerDeadline, hasDeadline := req.Context().Deadline()
		if !retryable || (req.Body != nil && req.GetBody == nil) ||
			(hasDeadline && time.Until(callerDeadline) <= delay) {
			if t.stream {
				_ = resp.Body.Close()
				return nil, &BusyStreamError{Message: message, RetryAfter: delay}
			}
			return resp, nil
		}
		_ = resp.Body.Close()
		resp.Body = http.NoBody
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			if errors.Is(req.Context().Err(), context.DeadlineExceeded) {
				return t.expiredBusy(lastResponse, lastMessage, lastDelay)
			}
			return nil, req.Context().Err()
		case <-timer.C:
		}
		request = req.Clone(req.Context())
		if req.Body != nil {
			request.Body, err = req.GetBody()
			if err != nil {
				return nil, err
			}
		}
	}
}

func (t busyTransport) expiredBusy(resp *http.Response, message string, delay time.Duration) (*http.Response, error) {
	if t.stream {
		return nil, &BusyStreamError{Message: message, RetryAfter: delay}
	}
	setBusyResponse(resp, message)
	return resp, nil
}

func setBusyResponse(resp *http.Response, message string) {
	encoded, _ := json.Marshal(api.NewError(http.StatusTooManyRequests, api.CodeServerBusy, message, "", nil).Envelope())
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.ContentLength = int64(len(encoded))
	resp.Header.Set("Content-Type", "application/json")
}
