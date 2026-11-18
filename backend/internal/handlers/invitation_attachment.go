package handlers

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"phase1-signora/backend/internal/services"
)

// Leave room under Brevo's 20 MB total message limit for base64, MIME and the email body.
const maxInvitationPDF = 13_000_000

const invitationPDFTooLarge = "PDF exceeds the 13 MB invitation attachment limit. Use the secure signing link to view it"
const invitationPDFUnavailable = "Original PDF could not be attached. Use the secure signing link to view it"

func (h SignersHandler) invitationAttachment(ctx context.Context, docID, userID, storedName, originalName string) ([]services.Attachment, string) {
	var data []byte
	var oversized bool
	err := h.DB.QueryRow(ctx, `
		SELECT CASE WHEN octet_length(file_data) <= $3 THEN file_data END,
		       COALESCE(octet_length(file_data) > $3, false)
		FROM documents WHERE id=$1 AND user_id=$2
	`, docID, userID, maxInvitationPDF).Scan(&data, &oversized)
	if err != nil {
		return nil, invitationPDFUnavailable
	}
	if oversized {
		return nil, invitationPDFTooLarge
	}
	return prepareInvitationPDF(data, h.UploadDir, storedName, originalName)
}

func prepareInvitationPDF(data []byte, uploadDir, storedName, originalName string) ([]services.Attachment, string) {
	if len(data) == 0 {
		// Older uploads may only exist on disk.
		if uploadDir == "" || storedName == "" || storedName == "." || storedName == ".." || filepath.Base(storedName) != storedName {
			return nil, invitationPDFUnavailable
		}
		file, err := os.Open(filepath.Join(uploadDir, storedName))
		if err != nil {
			return nil, invitationPDFUnavailable
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, maxInvitationPDF+1))
		if err != nil {
			return nil, invitationPDFUnavailable
		}
	}
	if len(data) > maxInvitationPDF {
		return nil, invitationPDFTooLarge
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, invitationPDFUnavailable
	}

	name := filepath.Base(strings.ReplaceAll(originalName, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < ' ' || r == 127 {
			return -1
		}
		return r
	}, name)
	if !strings.EqualFold(filepath.Ext(name), ".pdf") {
		name = "document.pdf"
	}
	return []services.Attachment{{Filename: name, Data: data}}, ""
}
