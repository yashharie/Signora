package handlers

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"phase1-signora/backend/internal/middleware"
	"phase1-signora/backend/internal/models"
	"phase1-signora/backend/internal/services"
	"phase1-signora/backend/internal/utils"
)

type StatusHandler struct {
	DB        *pgxpool.Pool
	UploadDir string
	SignedDir string
	Mailer    *services.Mailer
}

// queryAuditLogs returns the audit trail of a document, newest first (timeline order).
// Callers must have verified that the document belongs to the logged-in user.
func (h StatusHandler) queryAuditLogs(r *http.Request, docID string) ([]models.AuditLog, error) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, document_id, action, performed_by, COALESCE(details, ''), COALESCE(ip_address, ''), created_at
		FROM audit_logs
		WHERE document_id = $1
		ORDER BY created_at DESC, id DESC
	`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := []models.AuditLog{}
	for rows.Next() {
		var l models.AuditLog
		if err := rows.Scan(&l.ID, &l.DocumentID, &l.Action, &l.PerformedBy, &l.Details, &l.IPAddress, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// GetStatus returns the complete status, progress bar data, signers, boxes, and audit timeline
func (h StatusHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)

	var doc models.Document
	var signedFileName *string
	err := h.DB.QueryRow(r.Context(), `
		SELECT id, user_id, title, original_name, stored_name, signed_file_name, file_size, status, created_at
		FROM documents
		WHERE id = $1 AND user_id = $2
	`, docID, userID).Scan(
		&doc.ID, &doc.UserID, &doc.Title, &doc.OriginalName, &doc.StoredName,
		&signedFileName, &doc.FileSize, &doc.Status, &doc.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		utils.Error(w, 404, "Document not found")
		return
	}
	if err != nil {
		utils.Error(w, 500, "Could not load document")
		return
	}
	doc.SignedFileName = signedFileName

	// Query signers
	signerRows, err := h.DB.Query(r.Context(), `
		SELECT id, document_id, name, email, token, token_expires_at, status, signed_at, email_sent, email_sent_at, created_at
		FROM signers
		WHERE document_id = $1
		ORDER BY created_at ASC
	`, docID)
	if err != nil {
		utils.Error(w, 500, "Could not load signers")
		return
	}
	defer signerRows.Close()

	signers := []models.Signer{}
	signedCount := 0
	for signerRows.Next() {
		var s models.Signer
		var signedAt, emailSentAt *time.Time
		if err := signerRows.Scan(&s.ID, &s.DocumentID, &s.Name, &s.Email, &s.Token, &s.TokenExpiresAt, &s.Status, &signedAt, &s.EmailSent, &emailSentAt, &s.CreatedAt); err != nil {
			utils.Error(w, 500, "Could not read signers")
			return
		}
		s.SignedAt = signedAt
		s.EmailSentAt = emailSentAt
		if s.Status == "signed" {
			signedCount++
		}
		signers = append(signers, s)
	}
	if err := signerRows.Err(); err != nil {
		utils.Error(w, 500, "Could not read signers")
		return
	}

	// Query boxes
	boxRows, err := h.DB.Query(r.Context(), `
		SELECT b.id, b.document_id, b.signer_id, COALESCE(s.name, ''), COALESCE(s.email, ''),
		       b.page, b.x, b.y, b.width, b.height, b.signed, b.created_at
		FROM signature_boxes b
		LEFT JOIN signers s ON b.signer_id = s.id
		WHERE b.document_id = $1
		ORDER BY b.page ASC, b.created_at ASC
	`, docID)
	if err != nil {
		utils.Error(w, 500, "Could not load signature boxes")
		return
	}
	boxes := []models.SignatureBox{}
	defer boxRows.Close()
	for boxRows.Next() {
		var b models.SignatureBox
		var signerID *string
		if err := boxRows.Scan(&b.ID, &b.DocumentID, &signerID, &b.SignerName, &b.SignerEmail, &b.Page, &b.X, &b.Y, &b.Width, &b.Height, &b.Signed, &b.CreatedAt); err != nil {
			utils.Error(w, 500, "Could not read signature boxes")
			return
		}
		b.SignerID = signerID
		boxes = append(boxes, b)
	}
	if err := boxRows.Err(); err != nil {
		utils.Error(w, 500, "Could not read signature boxes")
		return
	}

	// Audit timeline (latest first, includes signer IP addresses)
	auditLogs, err := h.queryAuditLogs(r, docID)
	if err != nil {
		utils.Error(w, 500, "Could not load audit trail")
		return
	}

	totalSigners := len(signers)
	progress := 0
	if totalSigners > 0 {
		progress = (signedCount * 100) / totalSigners
	}

	// Self-heal: If all signers completed signing but document is still pending (e.g. from a prior transient stamper failure),
	// automatically finalize the document and generate the signed PDF now.
	if totalSigners > 0 && signedCount == totalSigners && doc.Status != "completed" {
		if err := services.CompleteDocument(r.Context(), h.DB, h.Mailer, h.UploadDir, h.SignedDir, docID); err != nil {
			// Log error but continue so the user still sees current progress and logs
			_ = err
		} else {
			doc.Status = "completed"
			_ = h.DB.QueryRow(r.Context(), `SELECT signed_file_name FROM documents WHERE id = $1`, docID).Scan(&signedFileName)
			doc.SignedFileName = signedFileName
		}
	}

	hasSignedPdf := doc.Status == "completed" && doc.SignedFileName != nil && *doc.SignedFileName != ""

	summary := models.DocumentStatusSummary{
		Document:     doc,
		Signers:      signers,
		Boxes:        boxes,
		AuditLogs:    auditLogs,
		TotalSigners: totalSigners,
		SignedCount:  signedCount,
		Progress:     progress,
		HasSignedPDF: hasSignedPdf,
	}

	utils.JSON(w, 200, summary)
}

// DownloadSigned downloads the completed, signed PDF
func (h StatusHandler) DownloadSigned(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)

	var status, originalName string
	var signedName *string
	err := h.DB.QueryRow(r.Context(), `
		SELECT status, original_name, signed_file_name
		FROM documents
		WHERE id = $1 AND user_id = $2
	`, docID, userID).Scan(&status, &originalName, &signedName)
	if err == pgx.ErrNoRows {
		utils.Error(w, 404, "Document not found")
		return
	}
	if err != nil {
		utils.Error(w, 500, "Could not load document")
		return
	}

	// Check if all signers have completed signing
	var totalSigners, pendingSigners int
	err = h.DB.QueryRow(r.Context(), `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status <> 'signed')
		FROM signers WHERE document_id = $1
	`, docID).Scan(&totalSigners, &pendingSigners)
	if err != nil {
		utils.Error(w, 500, "Could not verify signer progress")
		return
	}

	if totalSigners == 0 || pendingSigners > 0 {
		utils.Error(w, 400, "Signed PDF is only available after all signers have signed the document.")
		return
	}

	// If document is not marked completed or file is missing from disk, generate it automatically on the fly
	needGenerate := status != "completed" || signedName == nil || *signedName == ""
	var filePath string
	if !needGenerate {
		filePath = filepath.Join(h.SignedDir, filepath.Base(*signedName))
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			legacy := filepath.Join(h.UploadDir, filepath.Base(*signedName))
			if _, err2 := os.Stat(legacy); err2 == nil {
				filePath = legacy
			} else {
				needGenerate = true
			}
		}
	}

	if needGenerate {
		if err := services.CompleteDocument(r.Context(), h.DB, h.Mailer, h.UploadDir, h.SignedDir, docID); err != nil {
			utils.Error(w, 500, "Could not generate signed PDF: "+err.Error())
			return
		}
		_ = h.DB.QueryRow(r.Context(), `SELECT signed_file_name FROM documents WHERE id = $1`, docID).Scan(&signedName)
		if signedName == nil || *signedName == "" {
			utils.Error(w, 500, "Failed to retrieve generated signed PDF")
			return
		}
		filePath = filepath.Join(h.SignedDir, filepath.Base(*signedName))
	}

	baseName := strings.TrimSuffix(originalName, filepath.Ext(originalName))
	downloadName := fmt.Sprintf("%s_signed.pdf", baseName)

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(downloadName, "\"", "")))
	http.ServeFile(w, r, filePath)
}

// GetAuditLogs returns the audit trail for the document (owner only, latest first)
func (h StatusHandler) GetAuditLogs(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)

	// Verify doc ownership
	var exists bool
	err := h.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM documents WHERE id=$1 AND user_id=$2)`, docID, userID).Scan(&exists)
	if err != nil || !exists {
		utils.Error(w, 404, "Document not found")
		return
	}

	logs, err := h.queryAuditLogs(r, docID)
	if err != nil {
		utils.Error(w, 500, "Could not load audit logs")
		return
	}
	utils.JSON(w, 200, map[string]any{"auditLogs": logs})
}

// Regenerate rebuilds the signed PDF (and sends the completion email if it was never sent).
// Use it when all signers signed but PDF generation or the completion email failed.
func (h StatusHandler) Regenerate(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	userID := middleware.CurrentUserID(r)

	var status string
	err := h.DB.QueryRow(r.Context(), `SELECT status FROM documents WHERE id=$1 AND user_id=$2`, docID, userID).Scan(&status)
	if err == pgx.ErrNoRows {
		utils.Error(w, 404, "Document not found")
		return
	}
	if err != nil {
		utils.Error(w, 500, "Could not load document")
		return
	}
	if status == "draft" {
		utils.Error(w, 400, "The document has not been sent for signing yet")
		return
	}

	if err := services.CompleteDocument(r.Context(), h.DB, h.Mailer, h.UploadDir, h.SignedDir, docID); err != nil {
		utils.Error(w, 500, "Could not generate the signed PDF: "+shorten(err.Error(), 200))
		return
	}
	logAudit(r.Context(), h.DB, docID, "PDF_REGENERATED", userLabel(r.Context(), h.DB, userID),
		"Signed PDF was regenerated", utils.ClientIP(r))
	utils.JSON(w, 200, map[string]string{"message": "Signed PDF generated successfully"})
}
