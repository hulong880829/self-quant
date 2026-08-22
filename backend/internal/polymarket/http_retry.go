package polymarket

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

func doWithRetry(
	ctx context.Context,
	client *http.Client,
	newRequest func() (*http.Request, error),
	attempts int,
) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		request, buildErr := newRequest()
		if buildErr != nil {
			return nil, buildErr
		}
		response, err := client.Do(request.WithContext(ctx))
		if err == nil && response.StatusCode != http.StatusTooManyRequests &&
			response.StatusCode != http.StatusTooEarly &&
			response.StatusCode < http.StatusInternalServerError {
			return response, nil
		}
		if err != nil {
			lastErr = err
		} else if attempt == attempts-1 {
			return response, nil
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
		}
		if attempt == attempts-1 {
			break
		}
		delay := time.Duration(1<<attempt) * 200 * time.Millisecond
		if response != nil {
			retryAfter := response.Header.Get("Retry-After")
			if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil {
				delay = time.Duration(seconds) * time.Second
			} else if retryAt, dateErr := http.ParseTime(retryAfter); dateErr == nil {
				if until := time.Until(retryAt); until > 0 {
					delay = until
				}
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}
