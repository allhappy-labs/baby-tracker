package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxResponseBodyBytes int64 = 4 << 10

func Check(ctx context.Context, healthURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request health endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxResponseBodyBytes {
		return errors.New("health endpoint response exceeds 4 KiB")
	}
	bytesRead, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	if bytesRead > maxResponseBodyBytes {
		return errors.New("health endpoint response exceeds 4 KiB")
	}
	return nil
}
