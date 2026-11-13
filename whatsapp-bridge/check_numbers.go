package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
)

// POST /api/check-numbers: a read-only "is this number on WhatsApp" check.
//
// It uses whatsmeow's IsOnWhatsApp (a usync contact query). It sends no
// message, adds no contact and writes nothing to the message store. The one
// write that whatsmeow itself makes is its own LID<->phone cache in
// whatsapp.db, for numbers that are on WhatsApp.
//
// Account safety limits (KS-383, director order 2026-11-12 20:44):
//   - at most checkMaxNumbers (50) numbers per request, else 400 and no query;
//   - at most checkChunkSize (10) numbers per WhatsApp query;
//   - at least checkPace (2 s) between two WhatsApp queries, counted across
//     all requests, so two callers cannot add their rates together.
const (
	checkMaxNumbers = 50
	checkChunkSize  = 10
	checkPace       = 2 * time.Second
)

// waLookup is the part of types.IsOnWhatsAppResponse this endpoint reads.
type waLookup struct {
	Query string
	JID   string
	IsIn  bool
}

// waChecker asks WhatsApp about a list of phone numbers in digits.
type waChecker func(ctx context.Context, phones []string) ([]waLookup, error)

// clientChecker adapts a whatsmeow client to waChecker.
func clientChecker(client *whatsmeow.Client) waChecker {
	return func(ctx context.Context, phones []string) ([]waLookup, error) {
		res, err := client.IsOnWhatsApp(ctx, phones)
		out := make([]waLookup, 0, len(res))
		for _, r := range res {
			out = append(out, waLookup{
				Query: strings.TrimPrefix(r.Query, "+"),
				JID:   r.JID.String(),
				IsIn:  r.IsIn,
			})
		}
		return out, err
	}
}

// presenceLookup is the presence query the send path falls back on. It is a
// variable so a test can stand in for WhatsApp. It goes through the same
// process-wide pacer as /api/check-numbers.
var presenceLookup = func(ctx context.Context, client *whatsmeow.Client, phones []string) ([]waLookup, error) {
	if err := presencePacer.wait(ctx); err != nil {
		return nil, err
	}
	return clientChecker(client)(ctx, phones)
}

// presencePacer is the one gate for every presence query, check or send.
var presencePacer = newPacer(checkPace)

// pacer spaces WhatsApp queries: one process-wide gate.
type pacer struct {
	mu   sync.Mutex
	gap  time.Duration
	last time.Time
}

func newPacer(gap time.Duration) *pacer { return &pacer{gap: gap} }

// wait blocks until a query may go out, or the context ends.
func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.gap - time.Since(p.last); !p.last.IsZero() && d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.last = time.Now()
	return nil
}

type checkNumbersRequest struct {
	Numbers []string `json:"numbers"`
}

type checkNumberResult struct {
	Number     string `json:"number"`
	Checked    string `json:"checked,omitempty"`
	OnWhatsApp bool   `json:"on_whatsapp"`
	JID        string `json:"jid,omitempty"`
	// Matched says which form answered: "with_9" (a Brazilian 8-digit mobile
	// checked with the 9 in front), "as_given", or "" when not on WhatsApp.
	Matched string `json:"matched,omitempty"`
	Error   string `json:"error,omitempty"`
}

// brazilNine returns the form with the 9 in front when n is a Brazilian
// mobile written with 8 digits after the area code (55 + DD + 8 digits, the
// first of them 6 to 9). Fixed lines start with 2 to 5 and get no 9.
func brazilNine(n string) (string, bool) {
	if len(n) == 12 && strings.HasPrefix(n, "55") && n[4] >= '6' && n[4] <= '9' {
		return n[:4] + "9" + n[4:], true
	}
	return "", false
}

// normalizeCheckNumber keeps digits only (a leading + or spaces are allowed)
// and requires E.164 length, 8 to 15 digits.
func normalizeCheckNumber(raw string) (string, error) {
	var b strings.Builder
	for i, c := range strings.TrimSpace(raw) {
		switch {
		case c >= '0' && c <= '9':
			b.WriteRune(c)
		case c == '+' && i == 0, c == ' ', c == '-':
		default:
			return "", fmt.Errorf("not a phone number")
		}
	}
	d := b.String()
	if len(d) < 8 || len(d) > 15 {
		return "", fmt.Errorf("a number has 8 to 15 digits")
	}
	return d, nil
}

// queryAll asks WhatsApp about phones in chunks, one paced query per chunk.
func queryAll(ctx context.Context, check waChecker, p *pacer, phones []string) (map[string]waLookup, error) {
	found := map[string]waLookup{}
	for i := 0; i < len(phones); i += checkChunkSize {
		end := i + checkChunkSize
		if end > len(phones) {
			end = len(phones)
		}
		if err := p.wait(ctx); err != nil {
			return found, err
		}
		res, err := check(ctx, phones[i:end])
		if err != nil {
			return found, err
		}
		for _, r := range res {
			found[r.Query] = r
		}
	}
	return found, nil
}

func registerCheckNumbers(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, check waChecker, p *pacer) {
	mux.HandleFunc("/api/check-numbers", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req checkNumbersRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGroupError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		if len(req.Numbers) == 0 {
			writeGroupError(w, http.StatusBadRequest, "numbers is required")
			return
		}
		if len(req.Numbers) > checkMaxNumbers {
			writeGroupError(w, http.StatusBadRequest,
				fmt.Sprintf("at most %d numbers per request", checkMaxNumbers))
			return
		}
		fmt.Printf("→ /api/check-numbers from=%q count=%d\n", r.RemoteAddr, len(req.Numbers))

		results := make([]checkNumberResult, len(req.Numbers))
		primary := make([]string, len(req.Numbers)) // form asked first
		var round1 []string
		seen := map[string]bool{}
		for i, raw := range req.Numbers {
			results[i].Number = raw
			n, err := normalizeCheckNumber(raw)
			if err != nil {
				results[i].Error = err.Error()
				continue
			}
			primary[i] = n
			if nine, ok := brazilNine(n); ok {
				primary[i] = nine
			}
			if !seen[primary[i]] {
				seen[primary[i]] = true
				round1 = append(round1, primary[i])
			}
		}

		found, err := queryAll(r.Context(), check, p, round1)
		if err != nil {
			fmt.Printf("← /api/check-numbers error=%v\n", err)
			writeGroupError(w, http.StatusBadGateway, err.Error())
			return
		}

		// Round 2: a Brazilian 8-digit mobile not found with the 9 is asked
		// once more as given, because older accounts keep the 8-digit form.
		var round2 []string
		seen = map[string]bool{}
		for i, raw := range req.Numbers {
			if primary[i] == "" || found[primary[i]].IsIn {
				continue
			}
			n, _ := normalizeCheckNumber(raw)
			if n != primary[i] && !seen[n] {
				seen[n] = true
				round2 = append(round2, n)
			}
		}
		found2, err := queryAll(r.Context(), check, p, round2)
		if err != nil {
			fmt.Printf("← /api/check-numbers error=%v\n", err)
			writeGroupError(w, http.StatusBadGateway, err.Error())
			return
		}

		for i, raw := range req.Numbers {
			if primary[i] == "" {
				continue
			}
			n, _ := normalizeCheckNumber(raw)
			results[i].Checked = primary[i]
			if hit, ok := found[primary[i]]; ok && hit.IsIn {
				results[i].OnWhatsApp, results[i].JID = true, hit.JID
				results[i].Matched = "as_given"
				if primary[i] != n {
					results[i].Matched = "with_9"
				}
			} else if hit, ok := found2[n]; ok && hit.IsIn {
				results[i].Checked = n
				results[i].OnWhatsApp, results[i].JID = true, hit.JID
				results[i].Matched = "as_given"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
}
