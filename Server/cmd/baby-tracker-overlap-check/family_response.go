package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"babytracker/server/internal/protocol"
)

const overlapFamilyDisplayName = "Overlap family"

var overlapCanonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func verifyOverlapFamilyResponse(responsePath, headerPath, familyIDPath string, expectedStatus int, captureFamilyID bool) error {
	if expectedStatus != http.StatusOK && expectedStatus != http.StatusCreated {
		return errors.New("expected family HTTP status must be 200 or 201")
	}
	headerBytes, err := readBoundedOverlapFile(headerPath, 16<<10)
	if err != nil {
		return errors.New("read family response headers")
	}
	parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(headerBytes)), nil)
	if err != nil {
		return errors.New("parse family response headers")
	}
	_ = parsed.Body.Close()
	if parsed.StatusCode != expectedStatus || parsed.Header.Get("Cache-Control") != "no-store" {
		return errors.New("family response status or cache policy mismatch")
	}
	body, err := readBoundedOverlapFile(responsePath, 64<<10)
	if err != nil {
		return errors.New("read family response body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response protocol.FamilyResponse
	if err := decoder.Decode(&response); err != nil {
		return errors.New("decode family response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("family response has trailing content")
	}
	if response.ProtocolVersion != "v1" || !validOverlapUUID(response.ID) || response.DisplayName != overlapFamilyDisplayName ||
		response.Status != "active" || response.ProfileRequired || len(response.Members) < 1 || len(response.Members) > 2 {
		return errors.New("family response semantic mismatch")
	}
	owners, caregivers := 0, 0
	for _, member := range response.Members {
		if !validOverlapUUID(member.AccountID) || member.DisplayName != "Overlap caregiver" || member.Status != "active" {
			return errors.New("family member semantic mismatch")
		}
		switch member.Role {
		case "owner":
			owners++
		case "caregiver":
			caregivers++
		default:
			return errors.New("family role semantic mismatch")
		}
	}
	if owners != 1 || caregivers != len(response.Members)-1 {
		return errors.New("family owner semantic mismatch")
	}
	if captureFamilyID {
		if expectedStatus != http.StatusCreated {
			return errors.New("family ID capture requires created response")
		}
		if err := writeOverlapPrivateFile(familyIDPath, response.ID); err != nil {
			return errors.New("capture stable family ID")
		}
		return nil
	}
	expectedFamilyID, err := readBoundedOverlapFile(familyIDPath, 128)
	if err != nil || strings.TrimSpace(string(expectedFamilyID)) != response.ID {
		return errors.New("family ID changed across overlap phase")
	}
	return nil
}

func verifyOverlapInvitationResponse(responsePath, headerPath, familyIDPath, invitationIDPath string, expectedStatus int, captureInvitationID bool) error {
	if expectedStatus != http.StatusOK && expectedStatus != http.StatusCreated {
		return errors.New("expected invitation HTTP status must be 200 or 201")
	}
	headerBytes, err := readBoundedOverlapFile(headerPath, 16<<10)
	if err != nil {
		return errors.New("read invitation response headers")
	}
	parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(headerBytes)), nil)
	if err != nil {
		return errors.New("parse invitation response headers")
	}
	_ = parsed.Body.Close()
	if parsed.StatusCode != expectedStatus || parsed.Header.Get("Cache-Control") != "no-store" {
		return errors.New("invitation response status or cache policy mismatch")
	}
	body, err := readBoundedOverlapFile(responsePath, 64<<10)
	if err != nil {
		return errors.New("read invitation response body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response protocol.InvitationResponse
	if err := decoder.Decode(&response); err != nil {
		return errors.New("decode invitation response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invitation response has trailing content")
	}
	expectedFamilyID, err := readBoundedOverlapFile(familyIDPath, 128)
	if err != nil || response.ProtocolVersion != "v1" || !validOverlapUUID(response.ID) ||
		strings.TrimSpace(string(expectedFamilyID)) != response.FamilyID || response.Status != "active" || response.ExpiresAt == "" {
		return errors.New("invitation response semantic mismatch")
	}
	if captureInvitationID {
		if expectedStatus != http.StatusCreated || len(response.Token) != 43 || response.Link == "" || response.SecretUnavailable {
			return errors.New("created invitation omitted one-time secret")
		}
		return writeOverlapPrivateFile(invitationIDPath, response.ID)
	}
	expectedInvitationID, err := readBoundedOverlapFile(invitationIDPath, 128)
	if err != nil || strings.TrimSpace(string(expectedInvitationID)) != response.ID || response.Token != "" || response.Link != "" || !response.SecretUnavailable {
		return errors.New("invitation read did not preserve metadata-only semantics")
	}
	return nil
}

func verifyOverlapErrorResponse(responsePath, headerPath string, expectedStatus int, expectedCode string) error {
	if expectedStatus < 400 || expectedStatus > 599 || expectedCode == "" {
		return errors.New("invalid expected error response")
	}
	headerBytes, err := readBoundedOverlapFile(headerPath, 16<<10)
	if err != nil {
		return errors.New("read error response headers")
	}
	parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(headerBytes)), nil)
	if err != nil {
		return errors.New("parse error response headers")
	}
	_ = parsed.Body.Close()
	if parsed.StatusCode != expectedStatus || parsed.Header.Get("Cache-Control") != "no-store" {
		return errors.New("error response status or cache policy mismatch")
	}
	body, err := readBoundedOverlapFile(responsePath, 64<<10)
	if err != nil {
		return errors.New("read error response body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response protocol.ErrorResponse
	if err := decoder.Decode(&response); err != nil || response.Validate() != nil || string(response.Error.Code) != expectedCode || response.Error.Retryable {
		return errors.New("error response semantic mismatch")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("error response has trailing content")
	}
	return nil
}

func writeOverlapSessionCurlConfig(responsePath, curlConfigPath, familyIDPath string) error {
	body, err := readBoundedOverlapFile(responsePath, 64<<10)
	if err != nil {
		return errors.New("read session response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response protocol.SessionResponse
	expectedFamilyID, familyErr := readBoundedOverlapFile(familyIDPath, 128)
	if err := decoder.Decode(&response); err != nil || response.ProtocolVersion != "v1" || response.AccessToken == "" ||
		familyErr != nil || response.FamilySlot == nil || response.FamilySlot.State != "active" ||
		response.FamilySlot.FamilyID != strings.TrimSpace(string(expectedFamilyID)) {
		return errors.New("decode session response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("session response has trailing content")
	}
	return writeOverlapCurlConfig(curlConfigPath, response.AccessToken)
}

func validOverlapUUID(value string) bool {
	return overlapCanonicalUUID.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}

func readBoundedOverlapFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(content) == 0 || int64(len(content)) > maximum {
		return nil, errors.New("invalid bounded overlap file")
	}
	return content, nil
}

func writeOverlapPrivateFile(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(value); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
