package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/auth"
	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
)

func (a *App) setupForm(w http.ResponseWriter, r *http.Request) {
	if n, _ := a.store.CountAdmins(); n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.render(w, "setup.html", pageData{Title: "First-run setup"})
}

func (a *App) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if n, _ := a.store.CountAdmins(); n > 0 {
		http.Error(w, "setup already completed", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	confirm := r.FormValue("password_confirm")
	if err := security.ValidateAdminUsername(username); err != nil {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: err.Error()})
		return
	}
	if err := security.ValidateEmail(email); err != nil {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: err.Error()})
		return
	}
	if password != confirm {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: "Passwords do not match."})
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: err.Error()})
		return
	}
	var totpSecret, totpURL string
	enableTOTP := r.FormValue("enable_totp") == "1"
	if enableTOTP {
		totpSecret, totpURL, err = auth.GenerateTOTPSecret(username)
		if err != nil {
			a.render(w, "setup.html", pageData{Title: "First-run setup", Error: err.Error()})
			return
		}
	}
	id, err := a.store.CreateAdmin(username, email, hash, totpSecret, enableTOTP)
	if err != nil {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: "Could not create administrator: " + err.Error()})
		return
	}
	plains, hashes, err := auth.GenerateRecoveryCodes(10)
	if err != nil {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: err.Error()})
		return
	}
	if err := a.store.ReplaceRecoveryCodes(id, hashes); err != nil {
		a.render(w, "setup.html", pageData{Title: "First-run setup", Error: err.Error()})
		return
	}
	admin, _ := a.store.GetAdminByID(id)
	_ = a.store.AddAudit(id, "admin.setup", username, "first administrator created", clientIP(r))
	if err := a.startSession(w, r, admin); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "setup_done.html", pageData{
		Title: "Setup complete", Admin: admin,
		Data: map[string]any{"RecoveryCodes": plains, "TOTPSecret": totpSecret, "TOTPURL": totpURL},
	})
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if n, _ := a.store.CountAdmins(); n == 0 {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	a.render(w, "login.html", pageData{Title: "Login"})
}

const maxFailedLogins = 10

func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	ip := clientIP(r)
	if n, _ := a.store.RecentFailedLogins(username, ip, 15*time.Minute); n >= maxFailedLogins {
		_ = a.store.AddAudit(0, "auth.rate_limited", username, "too many failed logins", ip)
		a.render(w, "login.html", pageData{Title: "Login", Error: "Too many failed login attempts. Try again in 15 minutes."})
		return
	}
	admin, err := a.store.GetAdminByUsername(username)
	if err != nil || !auth.VerifyPassword(admin.PasswordHash, password) {
		_ = a.store.RecordLoginAttempt(username, ip, false)
		_ = a.store.AddAudit(0, "auth.login_failed", username, "invalid credentials", ip)
		a.render(w, "login.html", pageData{Title: "Login", Error: "Invalid username or password."})
		return
	}
	_ = a.store.RecordLoginAttempt(username, ip, true)
	if admin.TOTPEnabled {
		a.setPending(w, admin.ID)
		http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
		return
	}
	if err := a.startSession(w, r, admin); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = a.store.AddAudit(admin.ID, "auth.login", username, "login successful", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) totpForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.getPending(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.render(w, "totp.html", pageData{Title: "Two-factor authentication"})
}

func (a *App) totpSubmit(w http.ResponseWriter, r *http.Request) {
	adminID, ok := a.getPending(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	admin, err := a.store.GetAdminByID(adminID)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !auth.ValidateTOTP(admin.TOTPSecret, r.FormValue("code")) {
		_ = a.store.RecordLoginAttempt(admin.Username, clientIP(r), false)
		a.render(w, "totp.html", pageData{Title: "Two-factor authentication", Error: "Invalid code. Check your authenticator app and try again."})
		return
	}
	a.clearPending(w, r)
	if err := a.startSession(w, r, admin); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = a.store.AddAudit(admin.ID, "auth.login_totp", admin.Username, "login successful with TOTP", clientIP(r))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) recoverForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, "recover.html", pageData{Title: "Account recovery"})
}

func (a *App) recoverSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	code := r.FormValue("code")
	admin, err := a.store.GetAdminByUsername(username)
	if err != nil {
		a.render(w, "recover.html", pageData{Title: "Account recovery", Error: "Recovery failed. Check the username and code."})
		return
	}
	ok, err := a.store.ConsumeRecoveryCode(admin.ID, auth.RecoveryCodeHash(code))
	if err != nil || !ok {
		_ = a.store.RecordLoginAttempt(username, clientIP(r), false)
		a.render(w, "recover.html", pageData{Title: "Account recovery", Error: "Recovery failed. That code is invalid or already used."})
		return
	}
	// Recovery sign-in also disables TOTP until the admin re-enables it, and
	// kills every existing session - the safe response to a lost device.
	_ = a.store.UpdateAdminTOTP(admin.ID, "", false)
	_ = a.store.DeleteAllSessions(admin.ID)
	_ = a.store.AddAudit(admin.ID, "auth.recovery_used", username, "recovery code used; TOTP disabled; all sessions revoked", clientIP(r))
	if err := a.startSession(w, r, admin); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/account?flash="+"Recovery successful. Set a new password and re-enable two-factor authentication.", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("own_hpanel_session"); err == nil {
		_ = a.store.DeleteSession(auth.HashToken(c.Value))
	}
	a.audit(r, "auth.logout", "", "logout")
	http.SetCookie(w, &http.Cookie{Name: "own_hpanel_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) logoutAll(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	_ = a.store.DeleteAllSessions(admin.ID)
	a.audit(r, "auth.logout_all", "", "all sessions revoked")
	http.SetCookie(w, &http.Cookie{Name: "own_hpanel_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) generatePassword(w http.ResponseWriter, r *http.Request) {
	pw, err := auth.GenerateSecurePassword(24)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, pw)
}
