package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCustomEndpoints(t *testing.T) {
	const token = "test-token"
	root := t.TempDir()
	inside := filepath.Join(root, "photo.jpg")
	if err := os.WriteFile(inside, []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(outside, []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := []string{mustCanonical(t, root)}
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, token, roots)

	marshal := func(v interface{}) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	cases := []struct {
		name     string
		path     string
		body     string
		badToken bool
		want     int
	}{
		{"poll wrong token", "/api/poll", `{}`, true, http.StatusUnauthorized},
		{"poll too few options", "/api/poll", `{"recipient":"123","question":"q","options":["a"]}`, false, http.StatusBadRequest},
		{"poll blank option", "/api/poll", `{"recipient":"123","question":"q","options":["a"," "]}`, false, http.StatusBadRequest},
		{"poll disconnected", "/api/poll", `{"recipient":"123","question":"q","options":["a","b"]}`, false, http.StatusServiceUnavailable},
		{"create_group no name", "/api/create_group", `{"name":" "}`, false, http.StatusBadRequest},
		{"create_group disconnected", "/api/create_group", `{"name":"g","participants":["123"]}`, false, http.StatusServiceUnavailable},
		{"set_group_photo missing fields", "/api/set_group_photo", `{"group_jid":"1@g.us"}`, false, http.StatusBadRequest},
		{"set_group_photo outside roots", "/api/set_group_photo", marshal(map[string]string{"group_jid": "1@g.us", "photo_path": outside}), false, http.StatusForbidden},
		{"set_group_photo disconnected", "/api/set_group_photo", marshal(map[string]string{"group_jid": "1@g.us", "photo_path": inside}), false, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+tc.path, strings.NewReader(tc.body))
			if tc.badToken {
				req.Header.Set("Authorization", "Bearer wrong")
			} else {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusServiceUnavailable {
				var resp SendMessageResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Success || resp.Message != notConnectedMessage {
					t.Fatalf("unexpected not-connected body %q", rec.Body.String())
				}
			}
		})
	}
}

func mustCanonical(t *testing.T, p string) string {
	t.Helper()
	c, err := canonicalizePath(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
