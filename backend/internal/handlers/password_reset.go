package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"log"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"phase1-signora/backend/internal/utils"
)

const resetMessage = "If an account exists for that email, a password reset link will be sent."

func resetDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (h AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		utils.Error(w, 400, "Invalid request body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 255 {
		utils.Error(w, 400, "Enter a valid email address")
		return
	}
	// A per-account cooldown prevents repeated reset emails. Unknown accounts
	// receive the same response, including when SMTP delivery fails.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		utils.Error(w, 500, "Could not request password reset")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	digest := resetDigest(token)
	var userID string
	err = h.DB.QueryRow(r.Context(), `INSERT INTO password_resets(user_id,token_hash,expires_at)
 SELECT id,$2,NOW()+INTERVAL '30 minutes' FROM users WHERE email=$1
 ON CONFLICT(user_id) DO UPDATE SET token_hash=EXCLUDED.token_hash,
 expires_at=EXCLUDED.expires_at, requested_at=NOW()
 WHERE password_resets.requested_at < NOW()-INTERVAL '1 minute'
 RETURNING user_id`, email, digest).Scan(&userID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		utils.Error(w, 500, "Could not request password reset")
		return
	}
	if err == nil {
		link := strings.TrimRight(h.FrontendURL, "/") + "/reset-password#token=" + token
		text := "Reset your Signora password: " + link + "\nThis link expires in 30 minutes and can only be used once. If you did not request this, ignore this email."
		var sendErr error
		if h.Mailer == nil {
			sendErr = errors.New("mailer unavailable")
		} else {
			sendErr = h.Mailer.Send(email, "Reset your Signora password", `<p><a href="`+html.EscapeString(link)+`">Reset your password</a></p><p>This link expires in 30 minutes and can only be used once. If you did not request this, ignore this email.</p>`, text, nil)
		}
		if sendErr != nil {
			log.Print("Password reset email delivery failed")
			// Permit retry after delivery failure without deleting a newer request.
			_, _ = h.DB.Exec(r.Context(), `DELETE FROM password_resets WHERE user_id=$1 AND token_hash=$2`, userID, digest)
		}
	}
	utils.JSON(w, 200, map[string]string{"message": resetMessage})
}

func (h AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		utils.Error(w, 400, "Invalid request body")
		return
	}
	raw, err := hex.DecodeString(in.Token)
	if err != nil || len(raw) != 32 || in.Token != strings.ToLower(in.Token) {
		utils.Error(w, 400, "Invalid or expired reset link")
		return
	}
	if len(in.Password) < 8 || len(in.Password) > 72 {
		utils.Error(w, 400, "Password must be between 8 and 72 bytes")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		utils.Error(w, 500, "Could not secure password")
		return
	}
	// Consuming the token and changing the password are one atomic statement.
	result, err := h.DB.Exec(r.Context(), `WITH consumed AS (
 DELETE FROM password_resets WHERE token_hash=$1 AND expires_at>NOW() RETURNING user_id
 ) UPDATE users SET password_hash=$2 WHERE id IN (SELECT user_id FROM consumed)`, resetDigest(in.Token), string(hash))
	if err != nil {
		utils.Error(w, 500, "Could not reset password")
		return
	}
	if result.RowsAffected() != 1 {
		utils.Error(w, 400, "Invalid or expired reset link")
		return
	}
	utils.JSON(w, 200, map[string]string{"message": "Password updated. You can now log in."})
}
