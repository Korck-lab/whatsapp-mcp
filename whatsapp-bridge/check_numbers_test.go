package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// fakeWA answers the way IsOnWhatsApp does: one entry per number that exists
// on the fake network. It records every query so a test can read the traffic.
type fakeWA struct {
	mu      sync.Mutex
	on      map[string]string // digits -> jid
	queries [][]string
	times   []time.Time
}

func (f *fakeWA) check(_ context.Context, phones []string) ([]waLookup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, append([]string(nil), phones...))
	f.times = append(f.times, time.Now())
	var out []waLookup
	for _, p := range phones {
		if jid, ok := f.on[p]; ok {
			out = append(out, waLookup{Query: p, JID: jid, IsIn: true})
		} else {
			out = append(out, waLookup{Query: p, IsIn: false})
		}
	}
	return out, nil
}

func checkReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/check-numbers", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+testGroupToken)
	return req
}

func checkHandler(f *fakeWA, pace time.Duration) http.Handler {
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
	registerCheckNumbers(mux, auth, f.check, newPacer(pace))
	return mux
}

type checkAnswer struct {
	Results []struct {
		Number     string `json:"number"`
		Checked    string `json:"checked"`
		OnWhatsApp bool   `json:"on_whatsapp"`
		JID        string `json:"jid"`
		Matched    string `json:"matched"`
		Error      string `json:"error"`
	} `json:"results"`
}

func doCheck(t *testing.T, h http.Handler, body string) (int, checkAnswer) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, checkReq(body))
	var a checkAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	return rec.Code, a
}

func TestCheckNumbersYesAndNo(t *testing.T) {
	f := &fakeWA{on: map[string]string{"5511999990001": "5511999990001@s.whatsapp.net"}}
	code, a := doCheck(t, checkHandler(f, 0), `{"numbers":["5511999990001","5511999990002"]}`)
	if code != 200 || len(a.Results) != 2 {
		t.Fatalf("code=%d results=%d", code, len(a.Results))
	}
	if !a.Results[0].OnWhatsApp || a.Results[0].JID != "5511999990001@s.whatsapp.net" || a.Results[0].Matched != "as_given" {
		t.Fatalf("yes row wrong: %+v", a.Results[0])
	}
	if a.Results[1].OnWhatsApp || a.Results[1].JID != "" {
		t.Fatalf("no row wrong: %+v", a.Results[1])
	}
}

func TestCheckNumbersBrazilEightDigitsTriesNineFirst(t *testing.T) {
	f := &fakeWA{on: map[string]string{"5511987654321": "5511987654321@s.whatsapp.net"}}
	_, a := doCheck(t, checkHandler(f, 0), `{"numbers":["551187654321"]}`)
	r := a.Results[0]
	if !r.OnWhatsApp || r.Checked != "5511987654321" || r.Matched != "with_9" {
		t.Fatalf("expected with_9 match on 5511987654321, got %+v", r)
	}
	if f.queries[0][0] != "5511987654321" {
		t.Fatalf("the 9 form must be asked first, asked %v", f.queries[0])
	}
}

func TestCheckNumbersBrazilEightDigitsFallsBackToAsGiven(t *testing.T) {
	f := &fakeWA{on: map[string]string{"551187654321": "551187654321@s.whatsapp.net"}}
	_, a := doCheck(t, checkHandler(f, 0), `{"numbers":["551187654321"]}`)
	r := a.Results[0]
	if !r.OnWhatsApp || r.Matched != "as_given" || r.Checked != "551187654321" {
		t.Fatalf("expected as_given fallback, got %+v", r)
	}
}

func TestCheckNumbersLandlineGetsNoNine(t *testing.T) {
	f := &fakeWA{}
	doCheck(t, checkHandler(f, 0), `{"numbers":["551134567890"]}`)
	for _, q := range f.queries {
		for _, p := range q {
			if p != "551134567890" {
				t.Fatalf("a landline must not get a 9, asked %s", p)
			}
		}
	}
}

func TestCheckNumbersCapIs50(t *testing.T) {
	f := &fakeWA{}
	var nums []string
	for i := 0; i < 51; i++ {
		nums = append(nums, fmt.Sprintf(`"55119999%05d"`, i))
	}
	code, _ := doCheck(t, checkHandler(f, 0), `{"numbers":[`+strings.Join(nums, ",")+`]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("51 numbers must be 400, got %d", code)
	}
	if len(f.queries) != 0 {
		t.Fatalf("an over-cap request must reach WhatsApp zero times, reached %d", len(f.queries))
	}
}

func TestCheckNumbersRejectsBadInputPerNumber(t *testing.T) {
	f := &fakeWA{}
	code, a := doCheck(t, checkHandler(f, 0), `{"numbers":["abc","12"]}`)
	if code != 200 || a.Results[0].Error == "" || a.Results[1].Error == "" {
		t.Fatalf("bad numbers must carry an error each, got code=%d %+v", code, a)
	}
	if len(f.queries) != 0 {
		t.Fatal("invalid numbers must not reach WhatsApp")
	}
}

func TestCheckNumbersEmptyListAndMethodAndAuth(t *testing.T) {
	h := checkHandler(&fakeWA{}, 0)
	if code, _ := doCheck(t, h, `{"numbers":[]}`); code != http.StatusBadRequest {
		t.Fatalf("empty list must be 400, got %d", code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/check-numbers", nil)
	req.Header.Set("Authorization", "Bearer "+testGroupToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must be 405, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/check-numbers", strings.NewReader(`{"numbers":["5511999990001"]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token must be 401, got %d", rec.Code)
	}
}

func TestCheckNumbersPaceBetweenQueries(t *testing.T) {
	f := &fakeWA{}
	var nums []string
	for i := 0; i < 12; i++ { // 12 numbers = 2 queries of at most 10
		nums = append(nums, fmt.Sprintf(`"55119999%05d"`, i))
	}
	doCheck(t, checkHandler(f, 150*time.Millisecond), `{"numbers":[`+strings.Join(nums, ",")+`]}`)
	if len(f.times) < 2 {
		t.Fatalf("expected at least 2 queries, got %d", len(f.times))
	}
	if gap := f.times[1].Sub(f.times[0]); gap < 140*time.Millisecond {
		t.Fatalf("queries must be paced, gap was %v", gap)
	}
	for _, q := range f.queries {
		if len(q) > checkChunkSize {
			t.Fatalf("a query carried %d numbers, chunk is %d", len(q), checkChunkSize)
		}
	}
}

// recordingLIDStore is a LID store that remembers what is written to it.
type recordingLIDStore struct {
	mockLIDStore
	puts map[types.JID]types.JID // pn -> lid
}

func (r *recordingLIDStore) PutLIDMapping(_ context.Context, lid, pn types.JID) error {
	if r.puts == nil {
		r.puts = map[types.JID]types.JID{}
	}
	r.puts[pn] = lid
	return nil
}

// KS-384: GetUserInfo returns no LID for a never-chatted number, so the send
// path kept the phone JID and whatsmeow failed with "no LID found for <n>
// from server". IsOnWhatsApp (the check-numbers route) does return the LID.
func TestResolveRecipientUsesPresenceLookupWhenUserInfoHasNoLID(t *testing.T) {
	old, oldInfo := presenceLookup, userInfoLID
	defer func() { presenceLookup, userInfoLID = old, oldInfo }()
	userInfoLID = func(context.Context, *whatsmeow.Client, types.JID) (types.JID, error) {
		return types.JID{}, nil // what the server returns for a never-chatted number
	}
	presenceLookup = func(_ context.Context, _ *whatsmeow.Client, phones []string) ([]waLookup, error) {
		return []waLookup{{Query: phones[0], JID: "99887766@lid", IsIn: true}}, nil
	}
	st := &recordingLIDStore{}
	got, err := resolveRecipientJID(newTestClient(st), "5511999990001")
	if err != nil {
		t.Fatal(err)
	}
	want := types.NewJID("99887766", types.HiddenUserServer)
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
	pn := types.NewJID("5511999990001", types.DefaultUserServer)
	if st.puts[pn] != want {
		t.Fatalf("the pn->lid pair must be cached so whatsmeow finds it, got %v", st.puts)
	}
}

func TestResolveRecipientKeepsPhoneJIDWhenNotOnWhatsApp(t *testing.T) {
	old, oldInfo := presenceLookup, userInfoLID
	defer func() { presenceLookup, userInfoLID = old, oldInfo }()
	userInfoLID = func(context.Context, *whatsmeow.Client, types.JID) (types.JID, error) {
		return types.JID{}, nil // what the server returns for a never-chatted number
	}
	presenceLookup = func(_ context.Context, _ *whatsmeow.Client, phones []string) ([]waLookup, error) {
		return []waLookup{{Query: phones[0], IsIn: false}}, nil
	}
	st := &recordingLIDStore{}
	got, _ := resolveRecipientJID(newTestClient(st), "5511999990002")
	if got.Server != types.DefaultUserServer || len(st.puts) != 0 {
		t.Fatalf("a number not on WhatsApp must stay a phone JID and cache nothing, got %s %v", got, st.puts)
	}
}
