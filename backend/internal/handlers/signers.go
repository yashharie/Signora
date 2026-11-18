package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"phase1-signora/backend/internal/middleware"
	"phase1-signora/backend/internal/models"
	"phase1-signora/backend/internal/services"
	"phase1-signora/backend/internal/utils"
)

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// Signing links are valid for 7 days from the moment they are emailed.
const signingLinkTTL = 7 * 24 * time.Hour

type SignersHandler struct {
	DB          *pgxpool.Pool
	FrontendURL string
	UploadDir   string
	Mailer      *services.Mailer
}

type addSignerInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// List signers for a document
func (h SignersHandler) List(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)

	// Verify document belongs to user
	var exists bool
	err := h.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM documents WHERE id=$1 AND user_id=$2)`, docID, userID).Scan(&exists)
	if err != nil || !exists {
		utils.Error(w, 404, "Document not found")
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT id, document_id, name, email, token, token_expires_at, status, signed_at, email_sent, email_sent_at, created_at
		FROM signers
		WHERE document_id = $1
		ORDER BY created_at ASC
	`, docID)
	if err != nil {
		utils.Error(w, 500, "Could not load signers")
		return
	}
	defer rows.Close()

	signers := []models.Signer{}
	for rows.Next() {
		var s models.Signer
		var signedAt, emailSentAt *time.Time
		if err := rows.Scan(&s.ID, &s.DocumentID, &s.Name, &s.Email, &s.Token, &s.TokenExpiresAt, &s.Status, &signedAt, &s.EmailSent, &emailSentAt, &s.CreatedAt); err != nil {
			utils.Error(w, 500, "Could not read signers")
			return
		}
		s.SignedAt = signedAt
		s.EmailSentAt = emailSentAt
		signers = append(signers, s)
	}
	if err := rows.Err(); err != nil {
		utils.Error(w, 500, "Could not read signers")
		return
	}

	utils.JSON(w, 200, map[string]any{"signers": signers})
}

// Add signer to document
func (h SignersHandler) Add(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)

	var docStatus string
	err := h.DB.QueryRow(r.Context(), `SELECT status FROM documents WHERE id=$1 AND user_id=$2`, docID, userID).Scan(&docStatus)
	if err != nil {
		utils.Error(w, 404, "Document not found")
		return
	}
	if docStatus == "completed" {
		utils.Error(w, 400, "Cannot add signers to a completed document")
		return
	}

	var in addSignerInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		utils.Error(w, 400, "Invalid request body")
		return
	}

	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))

	if in.Name == "" {
		utils.Error(w, 400, "Signer name is required")
		return
	}
	if in.Email == "" || !emailRegex.MatchString(in.Email) {
		utils.Error(w, 400, "A valid signer email address is required")
		return
	}

	// Check duplicate email for this document
	var dup bool
	if err := h.DB.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM signers WHERE document_id=$1 AND email=$2)`,
		docID, in.Email).Scan(&dup); err != nil {
		utils.Error(w, 500, "Could not check for duplicate signer")
		return
	}
	if dup {
		utils.Error(w, 409, "A signer with this email address has already been added to this document")
		return
	}

	// Every signer gets their own random token. The 7-day window restarts when the email is sent.
	token, err := generateSecureToken()
	if err != nil {
		utils.Error(w, 500, "Could not create secure signing link")
		return
	}
	expiresAt := time.Now().Add(signingLinkTTL)

	var newSigner models.Signer
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO signers (document_id, name, email, token, token_expires_at, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')
		RETURNING id, document_id, name, email, token, token_expires_at, status, created_at
	`, docID, in.Name, in.Email, token, expiresAt).Scan(
		&newSigner.ID, &newSigner.DocumentID, &newSigner.Name, &newSigner.Email,
		&newSigner.Token, &newSigner.TokenExpiresAt, &newSigner.Status, &newSigner.CreatedAt,
	)
	if err != nil {
		utils.Error(w, 500, "Could not add signer")
		return
	}

	// Automatically transition document to pending so signing links are instantly live
	_, _ = h.DB.Exec(r.Context(), `UPDATE documents SET status='pending' WHERE id=$1 AND status='draft'`, docID)

	// Ensure the signer has a signature box ready. If none exists, auto-assign or create default
	var boxCount int
	_ = h.DB.QueryRow(r.Context(), `SELECT COUNT(*) FROM signature_boxes WHERE document_id=$1 AND signer_id=$2`, docID, newSigner.ID).Scan(&boxCount)
	if boxCount == 0 {
		var unassignedID string
		err := h.DB.QueryRow(r.Context(), `SELECT id FROM signature_boxes WHERE document_id=$1 AND signer_id IS NULL LIMIT 1`, docID).Scan(&unassignedID)
		if err == nil && unassignedID != "" {
			_, _ = h.DB.Exec(r.Context(), `UPDATE signature_boxes SET signer_id=$1 WHERE id=$2`, newSigner.ID, unassignedID)
		} else {
			_, _ = h.DB.Exec(r.Context(), `
				INSERT INTO signature_boxes (document_id, signer_id, page, x, y, width, height, signed)
				VALUES ($1, $2, 1, 20, 80, 35, 10, false)
			`, docID, newSigner.ID)
		}
	}

	logAudit(r.Context(), h.DB, docID, "SIGNER_ADDED", userLabel(r.Context(), h.DB, userID),
		fmt.Sprintf("Added signer %s (%s)", in.Name, in.Email), utils.ClientIP(r))

	baseURL := strings.TrimRight(h.FrontendURL, "/")
	signingURL := fmt.Sprintf("%s/sign/%s", baseURL, newSigner.Token)

	utils.JSON(w, 201, map[string]any{
		"message":    "Signer added successfully! Signing link is ready to share.",
		"signer":     newSigner,
		"signingUrl": signingURL,
	})
}

// Remove signer
func (h SignersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	signerID := r.PathValue("signerId")
	userID := middleware.CurrentUserID(r)

	// Verify doc ownership
	var docStatus string
	err := h.DB.QueryRow(r.Context(), `SELECT status FROM documents WHERE id=$1 AND user_id=$2`, docID, userID).Scan(&docStatus)
	if err != nil {
		utils.Error(w, 404, "Document not found")
		return
	}
	if docStatus == "completed" {
		utils.Error(w, 400, "Cannot remove signers after the document has been completed")
		return
	}

	var signerName, signerEmail string
	err = h.DB.QueryRow(r.Context(), `
		DELETE FROM signers WHERE id=$1 AND document_id=$2 RETURNING name, email
	`, signerID, docID).Scan(&signerName, &signerEmail)
	if err == pgx.ErrNoRows {
		utils.Error(w, 404, "Signer not found")
		return
	}
	if err != nil {
		utils.Error(w, 500, "Could not remove signer")
		return
	}

	// Unlink any signature boxes assigned to this signer
	_, _ = h.DB.Exec(r.Context(), `UPDATE signature_boxes SET signer_id=NULL WHERE signer_id=$1`, signerID)

	logAudit(r.Context(), h.DB, docID, "SIGNER_REMOVED", userLabel(r.Context(), h.DB, userID),
		fmt.Sprintf("Removed signer %s (%s)", signerName, signerEmail), utils.ClientIP(r))

	utils.JSON(w, 200, map[string]string{"message": "Signer removed successfully"})
}

type sendTarget struct {
	ID, Name, Email, Token string
	ExpiresAt              time.Time
	EmailSent              bool
	AttemptedAt            *time.Time
}

type emailResult struct {
	Name            string            `json:"name"`
	Email           string            `json:"email"`
	Success         bool              `json:"success"`
	AlreadySent     bool              `json:"alreadySent,omitempty"`
	AttachmentError string            `json:"attachmentError,omitempty"`
	Error           string            `json:"error,omitempty"`
	Outcome         invitationOutcome `json:"outcome"`
}

type invitationOutcome string

const (
	invitationSent           invitationOutcome = "sent"
	invitationFailed         invitationOutcome = "failed"
	invitationAlreadySent    invitationOutcome = "already_sent"
	invitationUnconfirmed    invitationOutcome = "unconfirmed"
	invitationSentWithoutPDF invitationOutcome = "sent_without_pdf"
)

type signerLink struct {
	SignerID   string `json:"signerId"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Token      string `json:"token"`
	SigningURL string `json:"signingUrl"`
}

// Send invites pending signers and reissues expired links.
func (h SignersHandler) Send(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)
	ip := utils.ClientIP(r)

	var docTitle, status, storedName, originalName string
	err := h.DB.QueryRow(ctx, `SELECT title, status, stored_name, original_name FROM documents WHERE id=$1 AND user_id=$2`, docID, userID).
		Scan(&docTitle, &status, &storedName, &originalName)
	if err != nil {
		utils.Error(w, 404, "Document not found")
		return
	}
	if status == "completed" {
		utils.Error(w, 400, "This document is already completed")
		return
	}

	// 1. Must have at least 1 signer
	var signerCount int
	err = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM signers WHERE document_id=$1`, docID).Scan(&signerCount)
	if err != nil {
		utils.Error(w, 500, "Could not verify document signers")
		return
	}
	if signerCount == 0 {
		utils.Error(w, 400, "Please add at least one signer before sending the document")
		return
	}

	// 2. Every box must be linked to a signer
	var unassignedBoxes int
	err = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM signature_boxes WHERE document_id=$1 AND signer_id IS NULL`, docID).Scan(&unassignedBoxes)
	if err != nil {
		utils.Error(w, 500, "Could not verify signature box assignments")
		return
	}
	if unassignedBoxes > 0 {
		utils.Error(w, 400, fmt.Sprintf("There are %d signature box(es) not yet assigned to a signer. Please assign all boxes first.", unassignedBoxes))
		return
	}
	var signersWithoutBoxes int
	err = h.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM signers s
		WHERE s.document_id=$1 AND s.status <> 'signed'
		  AND NOT EXISTS (
		    SELECT 1 FROM signature_boxes b
		    WHERE b.document_id=s.document_id AND b.signer_id=s.id
		  )
	`, docID).Scan(&signersWithoutBoxes)
	if err != nil {
		utils.Error(w, 500, "Could not verify signer signature boxes")
		return
	}
	if signersWithoutBoxes > 0 {
		utils.Error(w, 400, "Every pending signer must have at least one assigned signature box")
		return
	}

	// 3. Load the signers who still need to sign
	rows, err := h.DB.Query(ctx, `
		SELECT id, name, email, token, token_expires_at, email_sent, email_sent_at
		FROM signers
		WHERE document_id=$1 AND status <> 'signed'
		ORDER BY created_at ASC
	`, docID)
	if err != nil {
		utils.Error(w, 500, "Could not load signers")
		return
	}
	var targets []sendTarget
	for rows.Next() {
		var t sendTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Email, &t.Token, &t.ExpiresAt, &t.EmailSent, &t.AttemptedAt); err != nil {
			rows.Close()
			utils.Error(w, 500, "Could not read signers")
			return
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		utils.Error(w, 500, "Could not read signers")
		return
	}
	rows.Close()
	if len(targets) == 0 {
		utils.Error(w, 400, "All signers have already signed this document")
		return
	}

	sender := userLabel(ctx, h.DB, userID)
	var senderName string
	if err := h.DB.QueryRow(ctx, `SELECT name FROM users WHERE id=$1`, userID).Scan(&senderName); err != nil || senderName == "" {
		senderName = "A Signora user"
	}

	if status == "draft" {
		if _, err := h.DB.Exec(ctx, `UPDATE documents SET status='pending' WHERE id=$1 AND status='draft'`, docID); err != nil {
			utils.Error(w, 500, "Could not update document status")
			return
		}
		logAudit(ctx, h.DB, docID, "DOCUMENT_SENT", sender,
			fmt.Sprintf("Document sent to %d signer(s) for electronic signatures", len(targets)), ip)
	}

	baseURL := strings.TrimRight(h.FrontendURL, "/")
	results := make([]emailResult, 0, len(targets))
	links := make([]signerLink, 0, len(targets))
	sent, failed, skipped, attachmentFailures := 0, 0, 0, 0
	loadAttachment := sync.OnceValues(func() ([]services.Attachment, string) {
		return h.invitationAttachment(ctx, docID, userID, storedName, originalName)
	})

	for _, t := range targets {
		res := emailResult{Name: t.Name, Email: t.Email, Outcome: invitationFailed}
		current := t
		claimed := false
		if t.canClaim(time.Now()) {
			current, claimed, err = h.claimInvitation(ctx, docID, t)
			if err != nil {
				res.Error = "Could not prepare the invitation"
			}
		}
		link := fmt.Sprintf("%s/sign/%s", baseURL, current.Token)
		if current.Token != "" && time.Now().Before(current.ExpiresAt) {
			links = append(links, signerLink{SignerID: t.ID, Name: t.Name, Email: t.Email, Token: current.Token, SigningURL: link})
		}
		switch {
		case claimed:
			h.sendInvitation(ctx, current, senderName, docTitle, link, t.EmailSent, loadAttachment, &res)
		case res.Error != "":
		case current.EmailSent:
			res.Success, res.AlreadySent = true, true
			res.Outcome = invitationAlreadySent
		default:
			res.Outcome = invitationUnconfirmed
			res.Error = "An invitation is in progress or its delivery could not be confirmed. Share the signing link manually if needed"
		}
		results = append(results, res)

		switch {
		case res.AlreadySent:
			skipped++
		case res.Success:
			sent++
			action := "EMAIL_SENT"
			if t.EmailSent {
				action = "REMINDER_SENT"
			}
			logAudit(ctx, h.DB, docID, action, sender, fmt.Sprintf("Signing email accepted for %s (%s)", t.Name, t.Email), ip)
		default:
			failed++
			logAudit(ctx, h.DB, docID, "EMAIL_FAILED", sender, res.Error, ip)
		}
		if res.AttachmentError != "" {
			attachmentFailures++
			logAudit(ctx, h.DB, docID, "ATTACHMENT_FAILED", sender, res.AttachmentError, ip)
		}
	}

	msg := fmt.Sprintf("%d invitation(s) sent, %d already sent, %d failed or unconfirmed.", sent, skipped, failed)

	w.Header().Set("Cache-Control", "no-store")
	utils.JSON(w, 200, map[string]any{
		"message":            msg,
		"status":             "pending",
		"links":              links,
		"emailResults":       results,
		"emailsSent":         sent,
		"emailsFailed":       failed,
		"attachmentFailures": attachmentFailures,
	})
}

func (t sendTarget) canClaim(now time.Time) bool {
	return !now.Before(t.ExpiresAt) || (!t.EmailSent && t.AttemptedAt == nil)
}

func (h SignersHandler) claimInvitation(ctx context.Context, docID string, t sendTarget) (sendTarget, bool, error) {
	now := time.Now()
	token := t.Token
	if !now.Before(t.ExpiresAt) {
		var err error
		token, err = generateSecureToken()
		if err != nil {
			return t, false, err
		}
	}

	// A timestamp on an unsent row reserves delivery across requests and API instances.
	current := t
	err := h.DB.QueryRow(ctx, `
		UPDATE signers s
		SET token=$3, token_expires_at=$4, email_sent=false, email_sent_at=NOW()
		WHERE s.id=$1 AND s.document_id=$2 AND s.status <> 'signed' AND s.token=$5
		  AND (s.token_expires_at <= NOW() OR (NOT s.email_sent AND s.email_sent_at IS NULL))
		  AND EXISTS (SELECT 1 FROM signature_boxes b WHERE b.document_id=s.document_id AND b.signer_id=s.id)
		  AND EXISTS (SELECT 1 FROM documents d WHERE d.id=s.document_id AND d.status='pending')
		RETURNING token, token_expires_at, email_sent, email_sent_at
	`, t.ID, docID, token, now.Add(signingLinkTTL), t.Token).
		Scan(&current.Token, &current.ExpiresAt, &current.EmailSent, &current.AttemptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = h.DB.QueryRow(ctx, `SELECT token, token_expires_at, email_sent, email_sent_at
			FROM signers WHERE id=$1 AND document_id=$2 AND status <> 'signed'`, t.ID, docID).
			Scan(&current.Token, &current.ExpiresAt, &current.EmailSent, &current.AttemptedAt)
		if err != nil {
			return sendTarget{}, false, err
		}
		return current, false, nil
	}
	return current, err == nil, err
}

func (h SignersHandler) sendInvitation(ctx context.Context, target sendTarget, sender, title, link string, reminder bool, loadAttachment func() ([]services.Attachment, string), result *emailResult) {
	address, parseErr := mail.ParseAddress(target.Email)
	if parseErr != nil || address.Address != target.Email || len(target.Email) > 255 || !emailRegex.MatchString(target.Email) {
		result.Error = "Invalid signer email address"
	} else {
		attachments, attachmentError := loadAttachment()
		result.AttachmentError = attachmentError
		subject, htmlBody, textBody := services.SigningRequestEmail(target.Name, sender, title, link, target.ExpiresAt, reminder)
		sendInvitationEmail(h.Mailer.Send, target.Email, subject, htmlBody, textBody, attachments, result)
	}
	if result.Outcome == invitationUnconfirmed {
		return
	}

	// Record the outcome even if the client disconnected while the provider was sending.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	updated, writeErr := h.DB.Exec(finishCtx, `
		UPDATE signers SET email_sent=$4, email_sent_at=CASE WHEN $4 THEN NOW() ELSE NULL END
		WHERE id=$1 AND token=$2 AND email_sent_at=$3 AND NOT email_sent
	`, target.ID, target.Token, target.AttemptedAt, result.Success)
	if writeErr != nil || updated.RowsAffected() != 1 {
		log.Print("Could not record signing invitation outcome")
	}
}

func sendInvitationEmail(send func(string, string, string, string, []services.Attachment) error, to, subject, htmlBody, textBody string, attachments []services.Attachment, result *emailResult) {
	err := send(to, subject, htmlBody, textBody, attachments)
	if len(attachments) != 0 && services.IsAttachmentSizeRejection(err) {
		result.AttachmentError = "The mail provider rejected the PDF size. The invitation was retried with the signing link only"
		err = send(to, subject, htmlBody, textBody, nil)
	}
	switch {
	case errors.Is(err, services.ErrDeliveryUncertain):
		result.Outcome = invitationUnconfirmed
		result.Error = "Delivery could not be confirmed. Share the signing link manually if needed"
	case err != nil:
		result.Outcome = invitationFailed
		result.Error = "The mail provider rejected the invitation or could not send it"
	default:
		result.Success = true
		result.Outcome = invitationSent
		if result.AttachmentError != "" {
			result.Outcome = invitationSentWithoutPDF
		}
	}
}

func generateSecureToken() (string, error) {
	bytes := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
