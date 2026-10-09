package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"babytracker/server/internal/protocol"
)

func drainSyncHTTP(ctx context.Context, origin, curlConfigPath, cursorPath string) (int, int, error) {
	token, err := readOverlapBearerToken(curlConfigPath)
	if err != nil {
		return 0, 0, err
	}
	raw, err := readBoundedOverlapFile(cursorPath, 4096)
	if err != nil {
		return 0, 0, err
	}
	var starting protocol.Cursor
	if json.Unmarshal(raw, &starting) != nil || starting.Validate() != nil {
		return 0, 0, fmt.Errorf("invalid overlap cursor artifact")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var fixed protocol.Cursor
	var pageToken *string
	pages, changes := 0, 0
	for ; pages < 1000; pages++ {
		requestBody := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Mutations: []protocol.Mutation{}, PageToken: pageToken}
		if pageToken == nil {
			requestBody.Cursor = &starting
		}
		encoded, marshalErr := json.Marshal(requestBody)
		if marshalErr != nil {
			return pages, changes, marshalErr
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/v1/sync", bytes.NewReader(encoded))
		if requestErr != nil {
			return pages, changes, requestErr
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Baby-Tracker-Protocol", "v1")
		request.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
		response, requestErr := client.Do(request)
		if requestErr != nil {
			return pages, changes, requestErr
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
			return pages, changes, fmt.Errorf("sync drain response status=%d cache_safe=%t: %w", response.StatusCode, response.Header.Get("Cache-Control") == "no-store", readErr)
		}
		var decoded protocol.SyncResponse
		if err = json.Unmarshal(body, &decoded); err != nil || decoded.HighWater.Validate() != nil {
			return pages, changes, fmt.Errorf("invalid sync drain response: %w", err)
		}
		if pages == 0 {
			fixed = decoded.HighWater
		} else if decoded.HighWater != fixed {
			return pages, changes, fmt.Errorf("sync drain high water changed across pages")
		}
		changes += len(decoded.Changes)
		if decoded.NextPageToken == nil {
			pages++
			if changes == 0 {
				return pages, changes, fmt.Errorf("sync drain observed no predecessor import changes")
			}
			if err = writeOverlapCursor(cursorPath, fixed); err != nil {
				return pages, changes, err
			}
			return pages, changes, nil
		}
		next := *decoded.NextPageToken
		pageToken = &next
	}
	return pages, changes, fmt.Errorf("sync drain exceeded page bound")
}

func readOverlapBearerToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		const prefix = `header = "Authorization: Bearer `
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, `"`) {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), `"`), nil
		}
	}
	return "", fmt.Errorf("missing private HTTP credential")
}
