package main

// Fork-local REST endpoints that upstream does not ship: /api/poll,
// /api/create_group and /api/set_group_photo. Request and response JSON
// shapes match the fork's earlier fix/george-whatsapp-reconnect-20261012
// branch so existing local callers keep working. Kept in their own file so
// upstream merges of main.go stay small.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// SendPollRequest represents the request body for the send poll API.
type SendPollRequest struct {
	Recipient string   `json:"recipient"`
	Question  string   `json:"question"`
	Options   []string `json:"options"`
	Multiple  bool     `json:"multiple"`
}

const notConnectedMessage = "WhatsApp client is not connected. Please wait for reconnection."

// writeNotConnected mirrors the 503 response the upstream handlers return
// while the client is unpaired or reconnecting. Returns true when it wrote.
func writeNotConnected(w http.ResponseWriter, client *whatsmeow.Client) bool {
	if client != nil && client.IsConnected() {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: notConnectedMessage})
	return true
}

func sendWhatsAppPoll(client *whatsmeow.Client, messageStore *MessageStore, recipient, question string, options []string, multiple bool) (bool, string) {
	settingsLookupJID := types.NewJID(recipient, types.DefaultUserServer)
	if strings.Contains(recipient, "@") {
		parsed, err := types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
		settingsLookupJID = parsed
	}

	recipientJID, err := resolveRecipientJID(client, recipient)
	if err != nil {
		return false, err.Error()
	}

	selectableCount := 1
	if multiple {
		selectableCount = len(options)
	}
	msg := client.BuildPollCreation(question, options, selectableCount)

	settings, err := messageStore.GetChatEphemeralSettings(resolveUserJID(client, settingsLookupJID, types.EmptyJID).String())
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Sprintf("Error loading chat settings: %v", err)
	}
	if err == nil {
		applyChatEphemeralSettings(msg, settings)
	}

	resp, err := client.SendMessage(context.Background(), recipientJID, msg)
	if err != nil {
		return false, fmt.Sprintf("Error sending poll: %v", err)
	}
	return true, fmt.Sprintf("Poll sent: %s", resp.ID)
}

func registerCustomEndpoints(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, messageStore *MessageStore, allowedMediaRoots []string) {
	mux.HandleFunc("/api/poll", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req SendPollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.Recipient == "" || strings.TrimSpace(req.Question) == "" {
			http.Error(w, "Recipient and question are required", http.StatusBadRequest)
			return
		}
		if len(req.Options) < 2 || len(req.Options) > 12 {
			http.Error(w, "Polls require 2 to 12 options", http.StatusBadRequest)
			return
		}
		for _, option := range req.Options {
			if strings.TrimSpace(option) == "" {
				http.Error(w, "Poll options cannot be empty", http.StatusBadRequest)
				return
			}
		}
		if writeNotConnected(w, client) {
			return
		}

		success, message := sendWhatsAppPoll(client, messageStore, req.Recipient, req.Question, req.Options, req.Multiple)
		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: success, Message: message})
	}))

	mux.HandleFunc("/api/create_group", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Name         string   `json:"name"`
			Participants []string `json:"participants"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			http.Error(w, "Group name is required", http.StatusBadRequest)
			return
		}

		participants := make([]types.JID, 0, len(req.Participants))
		for _, p := range req.Participants {
			jid := types.NewJID(p, types.DefaultUserServer)
			if strings.Contains(p, "@") {
				parsed, err := types.ParseJID(p)
				if err != nil {
					http.Error(w, fmt.Sprintf("Invalid participant JID %q: %v", p, err), http.StatusBadRequest)
					return
				}
				jid = parsed
			}
			participants = append(participants, jid)
		}
		if writeNotConnected(w, client) {
			return
		}

		group, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{
			Name:         req.Name,
			Participants: participants,
		})

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("Failed to create group: %v", err),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"jid":     group.JID.String(),
			"name":    group.Name,
		})
	}))

	// photo_path must be a JPEG inside the configured media roots, the same
	// confinement /api/send applies to media_path.
	mux.HandleFunc("/api/set_group_photo", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			GroupJID  string `json:"group_jid"`
			PhotoPath string `json:"photo_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.GroupJID == "" || req.PhotoPath == "" {
			http.Error(w, "group_jid and photo_path are required", http.StatusBadRequest)
			return
		}

		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid group JID: %v", err), http.StatusBadRequest)
			return
		}

		photoPath, err := validateMediaPath(req.PhotoPath, allowedMediaRoots)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("photo_path rejected: %v", err),
			})
			return
		}
		photo, err := os.ReadFile(photoPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read photo: %v", err), http.StatusBadRequest)
			return
		}
		if writeNotConnected(w, client) {
			return
		}

		pictureID, err := client.SetGroupPhoto(context.Background(), jid, photo)

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("Failed to set group photo: %v", err),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":    true,
			"picture_id": pictureID,
		})
	}))
}
