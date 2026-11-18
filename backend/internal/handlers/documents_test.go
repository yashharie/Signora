package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"phase1-signora/backend/internal/middleware"
)

type documentViewDB struct {
	queryDB
	row func(context.Context, string, ...any) pgx.Row
}

func (db documentViewDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return db.row(ctx, sql, args...)
}

type documentViewRow func(...any) error

func (row documentViewRow) Scan(dest ...any) error {
	return row(dest...)
}

func TestParseUploadSigners(t *testing.T) {
	signers, err := parseUploadSigners(`[{"name":" Alice ","email":" ALICE@example.com "},{"name":"Bob","email":"bob@example.com"}]`)
	if err != nil || len(signers) != 2 || signers[0].Name != "Alice" || signers[0].Email != "alice@example.com" {
		t.Fatalf("signer details were not normalized: %v", err)
	}
	signers[0].Token = "test-token"
	data, err := json.Marshal(signers)
	if err != nil || !strings.Contains(string(data), `"name":"Alice","email":"alice@example.com","token":"test-token"`) {
		t.Fatal("signer fields did not serialize for atomic insertion")
	}
	for _, raw := range []string{
		`[]`, `null`, `{"email":"alice@example.com"}`,
		`[{"name":"Alice","email":"invalid"}]`,
		`[{"name":"Alice","email":"alice@example.com"},{"name":"Bob","email":"ALICE@example.com"}]`,
	} {
		if _, err := parseUploadSigners(raw); err == nil {
			t.Fatal("invalid signer list was accepted")
		}
	}
	if legacy, err := parseUploadSigners(""); err != nil || len(legacy) != 0 {
		t.Fatal("legacy upload without signer details should still work")
	}
}

func TestViewOriginalPDF(t *testing.T) {
	const dbPDF = "%PDF-1.7 database original"
	const diskPDF = "%PDF-1.7 legacy original"
	const dbError = "database error with private information"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.pdf"), []byte(diskPDF), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, stored, data, want, byteRange string
		readErr                             error
		status                              int
		download                            bool
	}{
		{name: "database preferred", stored: "legacy.pdf", data: dbPDF, want: dbPDF, status: 200},
		{name: "database original without local copy", stored: "missing.pdf", data: dbPDF, want: dbPDF, status: 200},
		{name: "legacy disk original", stored: "legacy.pdf", want: diskPDF, status: 200},
		{name: "database range", data: dbPDF, want: "%PDF-", byteRange: "bytes=0-4", status: 206},
		{name: "download", data: dbPDF, want: dbPDF, status: 200, download: true},
		{name: "missing original", stored: "missing.pdf", want: "Original PDF is unavailable", status: 404},
		{name: "inaccessible document", readErr: pgx.ErrNoRows, want: "Document not found", status: 404},
		{name: "database read failure", stored: "legacy.pdf", readErr: errors.New(dbError), want: "Could not read the original PDF from the database", status: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := DocumentHandler{UploadDir: dir, DB: documentViewDB{row: func(_ context.Context, sql string, args ...any) pgx.Row {
				if !strings.Contains(sql, "file_data") || !strings.Contains(sql, "user_id=$2") || len(args) != 2 || args[0] != "document" || args[1] != "owner" {
					t.Fatal("PDF lookup must be scoped to the document owner")
				}
				return documentViewRow(func(dest ...any) error {
					if tc.readErr != nil {
						return tc.readErr
					}
					*dest[0].(*string) = tc.stored
					*dest[1].(*string) = "original.pdf"
					*dest[2].(*[]byte) = []byte(tc.data)
					return nil
				})
			}}}
			req := httptest.NewRequest(http.MethodGet, "/api/documents/document/view", nil)
			req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, "owner"))
			req.SetPathValue("id", "document")
			req.Header.Set("Range", tc.byteRange)
			w := httptest.NewRecorder()
			if tc.download {
				h.Download(w, req)
			} else {
				h.View(w, req)
			}
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if tc.status < 400 {
				if w.Body.String() != tc.want || w.Header().Get("Content-Type") != "application/pdf" {
					t.Fatal("incorrect PDF response")
				}
				if tc.download && !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
					t.Fatal("download disposition missing")
				}
			} else if !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), dbError) {
				t.Fatal("incorrect or unsafe error response")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("viewing a database-backed PDF must not create a local copy")
	}
}
