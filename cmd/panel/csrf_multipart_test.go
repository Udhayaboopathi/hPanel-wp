package main

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/auth"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

// Regression test: the auth middleware's CSRF check must see the
// csrf_token field inside a MULTIPART body. Previously it called
// ParseForm only, which leaves r.Form query-only for multipart
// requests, so every authenticated file upload (File Manager,
// one-upload import) was rejected with 403.
func TestRequireAuthCSRFMultipart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateAdmin("root", "r@x.io", "hash", "", false); err != nil {
		t.Fatal(err)
	}
	admin, err := st.GetAdminByUsername("root")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	loginReq := httptest.NewRequest("GET", "/", nil)
	loginRec := httptest.NewRecorder()
	if err := app.startSession(loginRec, loginReq, admin); err != nil {
		t.Fatal(err)
	}
	var sessionCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == "own_hpanel_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie issued")
	}
	sess, err := st.GetSession(auth.HashToken(sessionCookie.Value))
	if err != nil {
		t.Fatal(err)
	}

	okHandler := app.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	post := func(contentType string, body *bytes.Buffer) int {
		req := httptest.NewRequest("POST", "/upload", body)
		req.Header.Set("Content-Type", contentType)
		req.AddCookie(sessionCookie)
		rec := httptest.NewRecorder()
		okHandler(rec, req)
		return rec.Code
	}

	// Multipart with the correct token in a body FIELD: must pass.
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("csrf_token", sess.CSRFToken)
	fw, _ := mw.CreateFormFile("file", "x.txt")
	_, _ = fw.Write([]byte("data"))
	_ = mw.Close()
	if code := post(mw.FormDataContentType(), &mp); code != http.StatusNoContent {
		t.Errorf("multipart + valid token: got %d, want 204", code)
	}

	// Multipart with a wrong token: must still be rejected.
	var mp2 bytes.Buffer
	mw2 := multipart.NewWriter(&mp2)
	_ = mw2.WriteField("csrf_token", "000000000000000000000000000000000000000000000000")
	_ = mw2.Close()
	if code := post(mw2.FormDataContentType(), &mp2); code != http.StatusForbidden {
		t.Errorf("multipart + wrong token: got %d, want 403", code)
	}

	// URL-encoded posts keep working.
	form := bytes.NewBufferString("csrf_token=" + sess.CSRFToken)
	if code := post("application/x-www-form-urlencoded", form); code != http.StatusNoContent {
		t.Errorf("urlencoded + valid token: got %d, want 204", code)
	}
}
