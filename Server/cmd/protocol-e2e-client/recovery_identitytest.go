//go:build identitytest

package main

import (
	"babytracker/server/internal/protocol"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type recoveryExpectation struct {
	Fault     string
	Parent    protocol.SessionResponse
	Child     protocol.RecoverableRefreshResponse
	AttemptID string
}

func recoveryInput(e recoveryExpectation) protocol.RecoverableRefreshRequest {
	return protocol.RecoverableRefreshRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.RefreshRecoveryCapabilities()}, RefreshToken: e.Parent.RefreshToken, RefreshAttemptID: e.AttemptID, DeviceContinuity: protocol.DeviceContinuityClaim{DeviceID: e.Parent.DeviceID, Credential: e.Parent.DeviceCredential}}
}

func recoveryFaultIndex(point string) (int, error) {
	for i, value := range []string{"lost_response", "before_authority_commit", "after_authority_commit", "after_projection"} {
		if point == value {
			return i + 8, nil
		}
	}
	return 0, errors.New("invalid recovery fault boundary")
}
func (c client) recoveryBootstrap(ctx context.Context, path, point string) error {
	index, err := recoveryFaultIndex(point)
	if err != nil {
		return err
	}
	state, err := readState(path)
	if err != nil {
		return err
	}
	parent, err := c.signIn(ctx, fmt.Sprintf("10000000-0000-4000-8000-%012d", index), "Recovery")
	if err != nil {
		return err
	}
	e := recoveryExpectation{Parent: parent, AttemptID: fmt.Sprintf("80000000-0000-4000-8000-%012d", index), Fault: point}
	if point == "lost_response" {
		if err = c.post(ctx, "/v1/auth/refresh-recoverable", "", recoveryInput(e), &e.Child); err != nil {
			return err
		}
		if e.Child.RefreshAttemptID != e.AttemptID || e.Child.ReauthenticateAt != parent.ReauthenticateAt || e.Child.AccessToken == parent.AccessToken || e.Child.RefreshToken == parent.RefreshToken {
			return errors.New("recovery bootstrap changed deadline or failed to rotate")
		}
	} else {
		raw, err := json.Marshal(recoveryInput(e))
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/auth/refresh-recoverable", bytes.NewReader(raw))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Identitytest-Recovery-Fault", point)
		response, err := c.http.Do(request)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != 503 {
			return fmt.Errorf("boundary %s status=%d want503", point, response.StatusCode)
		}
	}
	state.Recovery = &e
	if err = writeState(path, state); err != nil {
		return err
	}
	fmt.Printf("recovery Go bootstrap: boundary=%s attempt persisted privately; install response discarded\n", point)
	return nil
}

func (c client) recoveryVerify(ctx context.Context, path string) error {
	state, err := readState(path)
	if err != nil {
		return err
	}
	if state.Recovery == nil {
		return errors.New("missing recovery bootstrap")
	}
	e := *state.Recovery
	if _, err := recoveryFaultIndex(e.Fault); err != nil {
		return err
	}
	if protocol.ValidateRefreshRecoveryRequest(recoveryInput(e)) != nil || (e.Fault == "lost_response" && (e.Child.RefreshAttemptID != e.AttemptID || e.Child.AccessToken == "" || e.Child.RefreshToken == "")) {
		return errors.New("invalid recovery expectation")
	}
	for i := 0; i < 2; i++ {
		var got protocol.RecoverableRefreshResponse
		if err = c.post(ctx, "/v1/auth/refresh-recoverable", "", recoveryInput(e), &got); err != nil {
			return err
		}
		if i == 0 && e.Fault != "lost_response" {
			if got.ReauthenticateAt != e.Parent.ReauthenticateAt || got.AccessToken == e.Parent.AccessToken || got.RefreshToken == e.Parent.RefreshToken {
				return errors.New("fault retry changed original deadline or did not rotate")
			}
			e.Child = got
		}
		if got.AccessToken != e.Child.AccessToken || got.RefreshToken != e.Child.RefreshToken || got.ExpiresAt != e.Child.ExpiresAt || got.ReauthenticateAt != e.Child.ReauthenticateAt || got.RefreshAttemptID != e.AttemptID || got.DeviceID != e.Parent.DeviceID || got.Account.ID != e.Parent.Account.ID || got.RecoveryEpoch != e.Parent.RecoveryEpoch || got.ServerInstanceID != e.Child.ServerInstanceID {
			return errors.New("restarted process changed frozen child credentials or deadline")
		}
	}
	state.Recovery = &e
	if err = writeState(path, state); err != nil {
		return err
	}
	fmt.Println("recovery Go verify: restarted application/client recovered identical tokens, attempt, device and original deadline twice")
	return nil
}
