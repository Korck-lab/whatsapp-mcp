package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
)

// KS-385: POST /api/archive archives or unarchives a chat on the phone, with
// whatsmeow's appstate.BuildArchive sent through SendAppState.

type fakeAppState struct{ patches []appstate.PatchInfo }

func (f *fakeAppState) send(_ context.Context, p appstate.PatchInfo) error {
	f.patches = append(f.patches, p)
	return nil
}

func archiveHandler(f *fakeAppState) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testGroupToken {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	registerArchive(mux, auth, f.send)
	return mux
}

func postArchive(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, groupRequest("/api/archive", body))
	return rec
}

func TestArchiveSendsAnArchivePatch(t *testing.T) {
	f := &fakeAppState{}
	rec := postArchive(archiveHandler(f), `{"chat_jid":"5511999990001@s.whatsapp.net","archive":true}`)
	if rec.Code != 200 || len(f.patches) != 1 {
		t.Fatalf("code=%d patches=%d body=%s", rec.Code, len(f.patches), rec.Body.String())
	}
	m := f.patches[0].Mutations[0]
	if m.Index[0] != appstate.IndexArchive || m.Index[1] != "5511999990001@s.whatsapp.net" {
		t.Fatalf("wrong index %v", m.Index)
	}
	if !m.Value.GetArchiveChatAction().GetArchived() {
		t.Fatal("archive:true must send Archived=true")
	}
}

func TestArchiveFalseUnarchives(t *testing.T) {
	f := &fakeAppState{}
	postArchive(archiveHandler(f), `{"chat_jid":"5511999990001@s.whatsapp.net","archive":false}`)
	if len(f.patches) != 1 || f.patches[0].Mutations[0].Value.GetArchiveChatAction().GetArchived() {
		t.Fatal("archive:false must send Archived=false")
	}
}

func TestArchiveAcceptsBareNumber(t *testing.T) {
	f := &fakeAppState{}
	postArchive(archiveHandler(f), `{"chat_jid":"5511999990001","archive":true}`)
	if len(f.patches) != 1 || f.patches[0].Mutations[0].Index[1] != "5511999990001@s.whatsapp.net" {
		t.Fatalf("a bare number must become a phone JID, got %+v", f.patches)
	}
}

func TestArchiveRejectsBadInputAndMissingFlag(t *testing.T) {
	f := &fakeAppState{}
	h := archiveHandler(f)
	for _, body := range []string{`{}`, `{"chat_jid":"","archive":true}`, `{"chat_jid":"5511999990001"}`, `{"chat_jid":"x y@z","archive":true}`, `nope`} {
		if rec := postArchive(h, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", body, rec.Code)
		}
	}
	if len(f.patches) != 0 {
		t.Fatal("a rejected request must send nothing")
	}
}

func TestArchiveMethodAndAuth(t *testing.T) {
	h := archiveHandler(&fakeAppState{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/archive", nil)
	req.Header.Set("Authorization", "Bearer "+testGroupToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must be 405, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/archive", strings.NewReader(`{"chat_jid":"1","archive":true}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token must be 401, got %d", rec.Code)
	}
}
