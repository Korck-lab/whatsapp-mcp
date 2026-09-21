package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Group management endpoints.
//
// These live in their own file and attach through a single call in
// newRESTMux, so the upstream handler block stays as its author wrote it.
//
// Auth is the same wrapper every other endpoint uses: the caller passes the
// already-built `auth` closure, so bearer-token and Host checks apply here
// without being restated.

// GroupCreateRequest is the body of POST /api/group/create.
type GroupCreateRequest struct {
	Name string `json:"name"`
	// Participants accepts bare phone numbers ("5511999999999") or full
	// JIDs ("5511999999999@s.whatsapp.net"). The caller's own number is
	// added by WhatsApp and does not need to be listed.
	Participants []string `json:"participants"`
}

// GroupCreateResponse is returned by POST /api/group/create.
type GroupCreateResponse struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// GroupParticipantsRequest is the body of POST /api/group/participants.
type GroupParticipantsRequest struct {
	JID          string   `json:"jid"`
	Participants []string `json:"participants"`
	// Action is one of add, remove, promote, demote.
	Action string `json:"action"`
}

// GroupParticipantResult carries the per-participant outcome. WhatsApp can
// fail one participant while accepting the rest.
type GroupParticipantResult struct {
	JID   string `json:"jid"`
	Error int    `json:"error,omitempty"`
}

// GroupParticipantsResponse is returned by POST /api/group/participants.
type GroupParticipantsResponse struct {
	JID          string                   `json:"jid"`
	Action       string                   `json:"action"`
	Participants []GroupParticipantResult `json:"participants"`
}

// parseGroupJID parses a JID and requires it to be a group.
func parseGroupJID(raw string) (types.JID, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return types.EmptyJID, fmt.Errorf("jid is required")
	}
	jid, err := types.ParseJID(s)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("invalid jid %q: %v", s, err)
	}
	if jid.Server != types.GroupServer {
		return types.EmptyJID, fmt.Errorf("jid %q is not a group (expected server %q)", s, types.GroupServer)
	}
	return jid, nil
}

// parseParticipantChange maps the request's action onto whatsmeow's enum.
// The set is closed: an unknown action is refused rather than passed through,
// because whatsmeow would send it to WhatsApp as an opaque string.
func parseParticipantChange(raw string) (whatsmeow.ParticipantChange, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "add":
		return whatsmeow.ParticipantChangeAdd, nil
	case "remove":
		return whatsmeow.ParticipantChangeRemove, nil
	case "promote":
		return whatsmeow.ParticipantChangePromote, nil
	case "demote":
		return whatsmeow.ParticipantChangeDemote, nil
	default:
		return "", fmt.Errorf("unknown action %q: expected add, remove, promote or demote", raw)
	}
}

// GroupPhotoRequest is the body of POST /api/group/photo. Exactly one of
// ImageBase64 and CopyFrom must be set.
type GroupPhotoRequest struct {
	JID string `json:"jid"`
	// ImageBase64 is a base64-encoded JPEG.
	ImageBase64 string `json:"image_base64"`
	// CopyFrom is a chat JID whose current photo is copied onto JID.
	CopyFrom string `json:"copy_from"`
}

// GroupPhotoResponse is returned by POST /api/group/photo.
type GroupPhotoResponse struct {
	JID       string `json:"jid"`
	PictureID string `json:"picture_id"`
	Bytes     int    `json:"bytes"`
	Source    string `json:"source"`
}

// maxAvatarBytes bounds the copy-from download.
const maxAvatarBytes = 8 << 20 // 8 MiB

// maxGroupNameLen is WhatsApp's limit. A longer name is rejected by the
// server with a 406, so it is caught here to give the caller a clear error.
const maxGroupNameLen = 25

// parseParticipantJID accepts a bare phone number or a full JID.
func parseParticipantJID(raw string) (types.JID, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return types.EmptyJID, fmt.Errorf("empty participant")
	}
	if !strings.ContainsRune(s, '@') {
		return types.NewJID(s, types.DefaultUserServer), nil
	}
	jid, err := types.ParseJID(s)
	if err != nil {
		return types.EmptyJID, err
	}
	if jid.User == "" || jid.Server == "" {
		return types.EmptyJID, fmt.Errorf("incomplete JID %q", s)
	}
	return jid, nil
}

// writeGroupError sends a JSON error with the given status.
func writeGroupError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// registerGroupEndpoints attaches the group management handlers to the mux.
func registerGroupEndpoints(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client) {
	// Create a group. Returns the new group's JID.
	mux.HandleFunc("/api/group/create", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		fmt.Printf("→ /api/group/create from=%q user_agent=%q\n", r.RemoteAddr, r.UserAgent())

		var req GroupCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGroupError(w, http.StatusBadRequest, "Invalid request format")
			return
		}

		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeGroupError(w, http.StatusBadRequest, "name is required")
			return
		}
		if len([]rune(name)) > maxGroupNameLen {
			writeGroupError(w, http.StatusBadRequest,
				fmt.Sprintf("name is limited to %d characters", maxGroupNameLen))
			return
		}

		participants := make([]types.JID, 0, len(req.Participants))
		for _, raw := range req.Participants {
			jid, err := parseParticipantJID(raw)
			if err != nil {
				writeGroupError(w, http.StatusBadRequest,
					fmt.Sprintf("invalid participant %q: %v", raw, err))
				return
			}
			participants = append(participants, jid)
		}

		fmt.Printf("→ /api/group/create name=%q participants=%d\n", name, len(participants))

		info, err := client.CreateGroup(r.Context(), whatsmeow.ReqCreateGroup{
			Name:         name,
			Participants: participants,
		})
		if err != nil {
			fmt.Printf("← /api/group/create error=%v\n", err)
			writeGroupError(w, http.StatusInternalServerError, err.Error())
			return
		}

		fmt.Printf("← /api/group/create jid=%s\n", info.JID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GroupCreateResponse{
			JID:  info.JID.String(),
			Name: info.Name,
		})
	}))

	// Add, remove, promote or demote participants of an existing group.
	mux.HandleFunc("/api/group/participants", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		fmt.Printf("→ /api/group/participants from=%q user_agent=%q\n", r.RemoteAddr, r.UserAgent())

		var req GroupParticipantsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGroupError(w, http.StatusBadRequest, "Invalid request format")
			return
		}

		groupJID, err := parseGroupJID(req.JID)
		if err != nil {
			writeGroupError(w, http.StatusBadRequest, err.Error())
			return
		}

		action, err := parseParticipantChange(req.Action)
		if err != nil {
			writeGroupError(w, http.StatusBadRequest, err.Error())
			return
		}

		if len(req.Participants) == 0 {
			writeGroupError(w, http.StatusBadRequest, "participants is required")
			return
		}
		participants := make([]types.JID, 0, len(req.Participants))
		for _, raw := range req.Participants {
			jid, pErr := parseParticipantJID(raw)
			if pErr != nil {
				writeGroupError(w, http.StatusBadRequest,
					fmt.Sprintf("invalid participant %q: %v", raw, pErr))
				return
			}
			participants = append(participants, jid)
		}

		fmt.Printf("→ /api/group/participants jid=%s action=%s participants=%d\n",
			groupJID, action, len(participants))

		changed, err := client.UpdateGroupParticipants(r.Context(), groupJID, participants, action)
		if err != nil {
			fmt.Printf("← /api/group/participants error=%v\n", err)
			writeGroupError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// A participant can fail individually while the request as a whole
		// succeeds, so each outcome is reported rather than collapsed into
		// one boolean.
		results := make([]GroupParticipantResult, 0, len(changed))
		for _, p := range changed {
			res := GroupParticipantResult{JID: p.JID.String(), Error: p.Error}
			results = append(results, res)
		}

		fmt.Printf("← /api/group/participants ok results=%d\n", len(results))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GroupParticipantsResponse{
			JID:          groupJID.String(),
			Action:       string(action),
			Participants: results,
		})
	}))

	// Set a group's photo, either from supplied bytes or by copying the
	// photo of another chat the account can already see.
	mux.HandleFunc("/api/group/photo", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		fmt.Printf("→ /api/group/photo from=%q user_agent=%q\n", r.RemoteAddr, r.UserAgent())

		var req GroupPhotoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGroupError(w, http.StatusBadRequest, "Invalid request format")
			return
		}

		groupJID, err := parseGroupJID(req.JID)
		if err != nil {
			writeGroupError(w, http.StatusBadRequest, err.Error())
			return
		}

		hasBytes := strings.TrimSpace(req.ImageBase64) != ""
		hasCopy := strings.TrimSpace(req.CopyFrom) != ""
		switch {
		case hasBytes && hasCopy:
			writeGroupError(w, http.StatusBadRequest,
				"give either image_base64 or copy_from, not both")
			return
		case !hasBytes && !hasCopy:
			writeGroupError(w, http.StatusBadRequest,
				"image_base64 or copy_from is required")
			return
		}

		var avatar []byte
		source := ""
		if hasBytes {
			avatar, err = base64.StdEncoding.DecodeString(strings.TrimSpace(req.ImageBase64))
			if err != nil {
				writeGroupError(w, http.StatusBadRequest,
					fmt.Sprintf("image_base64 is not valid base64: %v", err))
				return
			}
			source = "image_base64"
		} else {
			sourceJID, sErr := parseCopyFromJID(req.CopyFrom)
			if sErr != nil {
				writeGroupError(w, http.StatusBadRequest, sErr.Error())
				return
			}
			avatar, err = fetchProfilePicture(r.Context(), client, sourceJID)
			if err != nil {
				fmt.Printf("← /api/group/photo copy_from=%s error=%v\n", sourceJID, err)
				writeGroupError(w, http.StatusBadGateway,
					fmt.Sprintf("could not read the photo of %s: %v", sourceJID, err))
				return
			}
			source = "copy_from:" + sourceJID.String()
		}

		// WhatsApp rejects non-JPEG avatars with ErrInvalidImageFormat.
		// Checking here turns a remote rejection into a local, readable one.
		if !isJPEG(avatar) {
			writeGroupError(w, http.StatusBadRequest,
				fmt.Sprintf("the image is not a JPEG (%d bytes, source %s)", len(avatar), source))
			return
		}

		fmt.Printf("→ /api/group/photo jid=%s source=%s bytes=%d\n", groupJID, source, len(avatar))

		pictureID, err := client.SetGroupPhoto(r.Context(), groupJID, avatar)
		if err != nil {
			fmt.Printf("← /api/group/photo error=%v\n", err)
			writeGroupError(w, http.StatusInternalServerError, err.Error())
			return
		}

		fmt.Printf("← /api/group/photo jid=%s picture_id=%s\n", groupJID, pictureID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GroupPhotoResponse{
			JID:       groupJID.String(),
			PictureID: pictureID,
			Bytes:     len(avatar),
			Source:    source,
		})
	}))
}

// parseCopyFromJID accepts any chat JID whose picture may be copied: a group
// or a user.
func parseCopyFromJID(raw string) (types.JID, error) {
	s := strings.TrimSpace(raw)
	jid, err := types.ParseJID(s)
	if err != nil || jid.User == "" || jid.Server == "" {
		return types.EmptyJID, fmt.Errorf("invalid copy_from %q", s)
	}
	return jid, nil
}

// isJPEG reports whether b starts with the JPEG SOI marker.
func isJPEG(b []byte) bool {
	return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

// fetchProfilePicture reads the full-resolution photo of a chat and returns
// its bytes. Preview is false so the copy is the full image and not the
// thumbnail.
func fetchProfilePicture(ctx context.Context, client *whatsmeow.Client, jid types.JID) ([]byte, error) {
	info, err := client.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: false})
	if err != nil {
		return nil, err
	}
	if info == nil || info.URL == "" {
		return nil, fmt.Errorf("%s has no profile picture", jid)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s returned HTTP %d", info.ID, resp.StatusCode)
	}

	// Cap the read: the bridge should not be turned into an unbounded
	// downloader by a redirect to something large.
	return io.ReadAll(io.LimitReader(resp.Body, maxAvatarBytes))
}
