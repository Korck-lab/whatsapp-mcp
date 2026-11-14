package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

// POST /api/archive {"chat_jid": "...", "archive": true|false}
//
// Archives or unarchives a chat on the account's phone, through whatsmeow's
// appstate.BuildArchive and SendAppState. It sends no message. Archiving also
// unpins the chat (whatsmeow's own rule). Same bearer auth as /api/send.

// appStateSender sends one app-state patch; a variable seam for tests.
type appStateSender func(ctx context.Context, patch appstate.PatchInfo) error

func clientAppState(client *whatsmeow.Client) appStateSender {
	return func(ctx context.Context, p appstate.PatchInfo) error {
		return client.SendAppState(ctx, p)
	}
}

type archiveRequest struct {
	ChatJID string `json:"chat_jid"`
	Archive *bool  `json:"archive"`
}

func registerArchive(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, send appStateSender) {
	mux.HandleFunc("/api/archive", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req archiveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGroupError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		raw := strings.TrimSpace(req.ChatJID)
		if raw == "" || req.Archive == nil {
			writeGroupError(w, http.StatusBadRequest, "chat_jid and archive are required")
			return
		}
		var jid types.JID
		if strings.Contains(raw, "@") {
			var err error
			if jid, err = types.ParseJID(raw); err != nil || strings.ContainsAny(raw, " \t") {
				writeGroupError(w, http.StatusBadRequest, "invalid chat_jid")
				return
			}
		} else {
			for _, c := range raw {
				if c < '0' || c > '9' {
					writeGroupError(w, http.StatusBadRequest, "invalid chat_jid")
					return
				}
			}
			jid = types.NewJID(raw, types.DefaultUserServer)
		}
		fmt.Printf("→ /api/archive from=%q chat=%s archive=%v\n", r.RemoteAddr, jid, *req.Archive)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := send(ctx, appstate.BuildArchive(jid, *req.Archive, time.Time{}, nil)); err != nil {
			fmt.Printf("← /api/archive error=%v\n", err)
			writeGroupError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"chat_jid": jid.String(), "archived": *req.Archive})
	}))
}
