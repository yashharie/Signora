package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"phase1-signora/backend/internal/middleware"
	"phase1-signora/backend/internal/utils"
)

type queryDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type DocumentHandler struct {
	DB        queryDB
	UploadDir string
}

const maxUpload = 20 << 20
const maxUploadSigners = 100

type uploadSigner struct {
	addSignerInput
	Token string `json:"token"`
}

func parseUploadSigners(raw string) ([]uploadSigner, error) {
	if raw == "" {
		return []uploadSigner{}, nil
	}
	var signers []addSignerInput
	if err := json.Unmarshal([]byte(raw), &signers); err != nil || len(signers) < 1 || len(signers) > maxUploadSigners {
		return nil, fmt.Errorf("Provide between 1 and %d signers", maxUploadSigners)
	}
	seen := make(map[string]bool, len(signers))
	result := make([]uploadSigner, 0, len(signers))
	for i, signer := range signers {
		signer.Name = strings.TrimSpace(signer.Name)
		signer.Email = strings.ToLower(strings.TrimSpace(signer.Email))
		if signer.Name == "" || utf8.RuneCountInString(signer.Name) > 120 || len(signer.Email) > 255 || !emailRegex.MatchString(signer.Email) || strings.ContainsRune(signer.Name, 0) {
			return nil, fmt.Errorf("Signer %d needs a valid name and email address", i+1)
		}
		if seen[signer.Email] {
			return nil, errors.New("Each signer must have a different email address")
		}
		seen[signer.Email] = true
		result = append(result, uploadSigner{addSignerInput: signer})
	}
	return result, nil
}

func (h DocumentHandler) Upload(w http.ResponseWriter, r *http.Request) {
	userID := middleware.CurrentUserID(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1024*1024)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		utils.Error(w, 413, "File is too large. Maximum size is 20MB")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		utils.Error(w, 400, "Document title is required")
		return
	}
	signers, err := parseUploadSigners(r.FormValue("signers"))
	if err != nil {
		utils.Error(w, 400, err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		utils.Error(w, 400, "PDF file is required")
		return
	}
	defer file.Close()
	if header.Size > maxUpload {
		utils.Error(w, 413, "File is too large. Maximum size is 20MB")
		return
	}
	if strings.ToLower(filepath.Ext(header.Filename)) != ".pdf" {
		utils.Error(w, 400, "Only PDF files are accepted")
		return
	}
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		utils.Error(w, 500, "Could not read PDF")
		return
	}
	size := int64(len(fileBytes))
	if size < 5 || string(fileBytes[:5]) != "%PDF-" {
		utils.Error(w, 400, "Uploaded file is not a valid PDF")
		return
	}
	for i := range signers {
		signers[i].Token, err = generateSecureToken()
		if err != nil {
			utils.Error(w, 500, "Could not create secure signing links")
			return
		}
	}
	signerData, err := json.Marshal(signers)
	if err != nil {
		utils.Error(w, 500, "Could not prepare document signers")
		return
	}

	if err = os.MkdirAll(h.UploadDir, 0755); err != nil {
		utils.Error(w, 500, "Storage is unavailable")
		return
	}
	id := uuid.NewString()
	stored := id + ".pdf"
	dest := filepath.Join(h.UploadDir, stored)
	if err := os.WriteFile(dest, fileBytes, 0644); err != nil {
		utils.Error(w, 500, "Could not save PDF")
		return
	}

	var docID string
	err = h.DB.QueryRow(r.Context(), `
		WITH uploaded AS (
			INSERT INTO documents(id, user_id, title, original_name, stored_name, file_size, status, file_data)
			VALUES($1, $2, $3, $4, $5, $6, 'draft', $7)
			RETURNING id
		), initial_signers AS (
			INSERT INTO signers(document_id, name, email, token, token_expires_at)
			SELECT uploaded.id, signer.name, signer.email, signer.token, $9
			FROM uploaded CROSS JOIN jsonb_to_recordset($8::jsonb) AS signer(name text, email text, token text)
		)
		SELECT id FROM uploaded
	`, id, userID, title, filepath.Base(header.Filename), stored, size, fileBytes, string(signerData), time.Now().Add(signingLinkTTL)).Scan(&docID)
	if err != nil {
		_ = os.Remove(dest)
		utils.Error(w, 500, "Could not save document details")
		return
	}
	_, _ = h.DB.Exec(r.Context(), `
		INSERT INTO audit_logs (document_id, action, performed_by, details)
		VALUES ($1, 'DOCUMENT_UPLOADED', 'Sender', $2)
	`, docID, fmt.Sprintf("Document uploaded: %s (%s)", title, filepath.Base(header.Filename)))
	if len(signers) > 0 {
		logAudit(r.Context(), h.DB, docID, "SIGNER_ADDED", userLabel(r.Context(), h.DB, userID),
			fmt.Sprintf("Added %d signer(s) during upload", len(signers)), utils.ClientIP(r))
	}
	utils.JSON(w, 201, map[string]any{"message": "PDF uploaded successfully", "document": map[string]any{"id": docID, "title": title, "originalName": filepath.Base(header.Filename), "fileSize": size, "status": "draft"}})
}
func (h DocumentHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(r.Context(), `SELECT id,user_id,title,original_name,file_size,status,created_at FROM documents WHERE user_id=$1 ORDER BY created_at DESC`, middleware.CurrentUserID(r))
	if err != nil {
		utils.Error(w, 500, "Could not load documents")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, uid, title, name, status string
		var size int64
		var created any
		if err = rows.Scan(&id, &uid, &title, &name, &size, &status, &created); err != nil {
			utils.Error(w, 500, "Could not read documents")
			return
		}
		items = append(items, map[string]any{"id": id, "userId": uid, "title": title, "originalName": name, "fileSize": size, "status": status, "createdAt": created})
	}
	utils.JSON(w, 200, map[string]any{"documents": items})
}
func (h DocumentHandler) View(w http.ResponseWriter, r *http.Request)     { h.serveFile(w, r, false) }
func (h DocumentHandler) Download(w http.ResponseWriter, r *http.Request) { h.serveFile(w, r, true) }
func (h DocumentHandler) serveFile(w http.ResponseWriter, r *http.Request, download bool) {
	id := r.PathValue("id")
	var stored, name string
	var data []byte
	err := h.DB.QueryRow(r.Context(), `SELECT stored_name,original_name,file_data FROM documents WHERE id=$1 AND user_id=$2`, id, middleware.CurrentUserID(r)).Scan(&stored, &name, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		utils.Error(w, 404, "Document not found")
		return
	}
	if err != nil {
		utils.Error(w, 500, "Could not read the original PDF from the database")
		return
	}

	var content io.ReadSeeker = bytes.NewReader(data)
	var modified time.Time
	if len(data) == 0 {
		if stored == "" || stored == "." || stored == ".." || filepath.Base(stored) != stored {
			utils.Error(w, 404, "Original PDF is unavailable")
			return
		}
		file, err := os.Open(filepath.Join(h.UploadDir, stored))
		if errors.Is(err, os.ErrNotExist) {
			utils.Error(w, 404, "Original PDF is unavailable")
			return
		}
		if err != nil {
			utils.Error(w, 500, "Could not read the original PDF from storage")
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			utils.Error(w, 500, "Could not read the original PDF from storage")
			return
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			utils.Error(w, 404, "Original PDF is unavailable")
			return
		}
		content, modified = file, info.ModTime()
	}

	w.Header().Set("Content-Type", "application/pdf")
	disposition := "inline"
	if download {
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": name})
	}
	w.Header().Set("Content-Disposition", disposition)
	http.ServeContent(w, r, name, modified, content)
}
func (h DocumentHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var stored string
	err := h.DB.QueryRow(r.Context(), `DELETE FROM documents WHERE id=$1 AND user_id=$2 RETURNING stored_name`, id, middleware.CurrentUserID(r)).Scan(&stored)
	if err == pgx.ErrNoRows {
		utils.Error(w, 404, "Document not found")
		return
	}
	if err != nil {
		utils.Error(w, 500, "Could not delete document")
		return
	}
	_ = os.Remove(filepath.Join(h.UploadDir, filepath.Base(stored)))
	utils.JSON(w, 200, map[string]string{"message": "Document deleted successfully"})
}
