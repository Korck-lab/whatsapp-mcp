package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The group endpoints are tested at the same seam the repo already uses for
// handlers: newRESTMux driven through httptest. That boundary is reachable
// without a connected whatsmeow client, so these tests cover auth and input
// validation. The calls that reach WhatsApp itself are verified live against
// the running bridge, not here.

const testGroupToken = "test-token-group-endpoints"

// groupRequest builds an authenticated POST against the given path.
func groupRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+testGroupToken)
	return req
}

func TestGroupCreateRejectsMissingName(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, groupRequest("/api/group/create", `{"participants":["5511999999999"]}`))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a create with no name, got %d (body %q)", resp.Code, resp.Body.String())
	}
}

func TestGroupCreateRequiresAuth(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/group/create", strings.NewReader(`{"name":"x"}`))
	req.RemoteAddr = "127.0.0.1:54321"
	// deliberately no Authorization header
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a bearer token, got %d", resp.Code)
	}
}

func TestGroupParticipantsRejectsUnknownAction(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, groupRequest("/api/group/participants",
		`{"jid":"120363000000000000@g.us","participants":["5511999999999"],"action":"banish"}`))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown action, got %d (body %q)", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "banish") {
		t.Fatalf("expected the rejected action to be named in the error, got %q", resp.Body.String())
	}
}

func TestGroupParticipantsRejectsNonGroupJID(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	// A user JID is well-formed but is not a group, and sending participant
	// changes to it would be a silent no-op rather than an error.
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, groupRequest("/api/group/participants",
		`{"jid":"5511999999999@s.whatsapp.net","participants":["5511888888888"],"action":"add"}`))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-group jid, got %d (body %q)", resp.Code, resp.Body.String())
	}
}

func TestGroupPhotoRequiresASource(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, groupRequest("/api/group/photo",
		`{"jid":"120363000000000000@g.us"}`))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when neither image nor copy_from is given, got %d (body %q)",
			resp.Code, resp.Body.String())
	}
}

func TestGroupPhotoRejectsTwoSources(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	// Two sources is ambiguous: silently preferring one would make the
	// caller's intent unreadable from the request.
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, groupRequest("/api/group/photo",
		`{"jid":"120363000000000000@g.us","image_base64":"/9j/4AAQ","copy_from":"120363000000000001@g.us"}`))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when both image and copy_from are given, got %d (body %q)",
			resp.Code, resp.Body.String())
	}
}

func TestGroupPhotoRejectsNonJPEGBytes(t *testing.T) {
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, testGroupToken, nil)

	// "aGVsbG8=" is "hello": valid base64, not a JPEG. WhatsApp rejects
	// non-JPEG avatars, so the bridge should say so before the round trip.
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, groupRequest("/api/group/photo",
		`{"jid":"120363000000000000@g.us","image_base64":"aGVsbG8="}`))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-JPEG bytes, got %d (body %q)", resp.Code, resp.Body.String())
	}
}
