package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"phase1-signora/backend/internal/middleware"
	"phase1-signora/backend/internal/services"
)

const (
	firstSigner   = "00000000-0000-4000-8000-000000000001"
	secondSigner  = "00000000-0000-4000-8000-000000000002"
	thirdSigner   = "00000000-0000-4000-8000-000000000003"
	missingSigner = "00000000-0000-4000-8000-000000000004"
	testPDF       = "%PDF-1.7 original document"
)

func scanInvitationValues(values ...any) documentViewRow {
	return func(dest ...any) error {
		for i, value := range values {
			reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
		}
		return nil
	}
}

type selectedSignerRows struct {
	pgx.Rows
	rows []documentViewRow
}

func (r *selectedSignerRows) Next() bool { return len(r.rows) > 0 }
func (r *selectedSignerRows) Scan(dest ...any) error {
	row := r.rows[0]
	r.rows = r.rows[1:]
	return row.Scan(dest...)
}
func (r *selectedSignerRows) Err() error { return nil }
func (r *selectedSignerRows) Close()     {}

type selectedSignerDB struct {
	t       *testing.T
	targets map[string]*sendTarget
}

func (db selectedSignerDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT title, status"):
		if !strings.Contains(sql, "user_id=$2") || args[0] != "document" {
			db.t.Fatal("document lookup was not owner scoped")
		}
		if args[1] != "owner" {
			return documentViewRow(func(...any) error { return pgx.ErrNoRows })
		}
		return scanInvitationValues("Contract", "pending", "stored.pdf", "contract.pdf")
	case strings.Contains(sql, "SELECT name, email FROM users"):
		return scanInvitationValues("Sender", "sender@example.com")
	case strings.Contains(sql, "SELECT name FROM users"):
		return scanInvitationValues("Sender")
	case strings.Contains(sql, "octet_length(file_data)"):
		return scanInvitationValues([]byte(testPDF), false)
	case strings.Contains(sql, "UPDATE signers s"):
		target := db.targets[args[0].(string)]
		if !strings.Contains(sql, "b.signer_id=s.id") || !strings.Contains(sql, "d.user_id=$6") ||
			args[1] != "document" || args[5] != "owner" || !target.HasBox || target.EmailSent || target.AttemptedAt != nil {
			db.t.Fatal("unexpected invitation claim")
		}
		now := time.Now()
		target.AttemptedAt = &now
		return scanInvitationValues(target.Token, target.ExpiresAt, false, target.AttemptedAt)
	default:
		db.t.Fatalf("unexpected row query: %s", sql)
		return nil
	}
}

func (db selectedSignerDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "s.id=ANY(") || !strings.Contains(sql, "s.status='pending'") ||
		!strings.Contains(sql, "s.document_id=$1") || args[0] != "document" {
		db.t.Fatal("signer lookup was not scoped to the pending selection")
	}
	rows := &selectedSignerRows{}
	for _, id := range args[1].([]string) {
		if s := db.targets[id]; s != nil {
			rows.rows = append(rows.rows, scanInvitationValues(s.ID, s.Name, s.Email, s.Token, s.ExpiresAt, s.EmailSent, s.AttemptedAt, s.HasBox))
		}
	}
	return rows, nil
}

func (db selectedSignerDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE signers") {
		db.targets[args[0].(string)].EmailSent = args[3].(bool)
	} else if !strings.Contains(sql, "INSERT INTO audit_logs") {
		db.t.Fatalf("unexpected statement: %s", sql)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func TestSendOnlySelectedSigners(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	db := selectedSignerDB{t: t, targets: map[string]*sendTarget{
		firstSigner:  {ID: firstSigner, Email: "first@example.com", Token: "first-token", ExpiresAt: expires, HasBox: true},
		secondSigner: {ID: secondSigner, Email: "second@example.com", Token: "second-token", ExpiresAt: expires, HasBox: true},
		thirdSigner:  {ID: thirdSigner, Email: "third@example.com", Token: "third-token", ExpiresAt: expires},
	}}
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var recipients []string
	http.DefaultTransport = invitationTransport(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			To         []struct{ Email string }
			Attachment []struct {
				Name    string
				Content []byte
			}
			Subject, HTMLContent, TextContent string
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if req.URL.Host != "api.brevo.com" || len(payload.To) != 1 || len(payload.Attachment) != 1 ||
			payload.Attachment[0].Name != "contract.pdf" || string(payload.Attachment[0].Content) != testPDF ||
			!strings.Contains(payload.Subject, "Contract") {
			t.Fatal("incorrect Brevo delivery or original PDF")
		}
		email := payload.To[0].Email
		token := map[string]string{"first@example.com": "first-token", "second@example.com": "second-token"}[email]
		link := "https://signora.example/sign/" + token
		if token == "" || !strings.Contains(payload.TextContent, link) || !strings.Contains(payload.HTMLContent, link) {
			t.Fatal("unexpected signer or signing link")
		}
		recipients = append(recipients, email)
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	h := SignersHandler{DB: db, FrontendURL: "https://signora.example",
		Mailer: &services.Mailer{DeliveryProvider: "brevo", BrevoKey: "test-key", BrevoSender: "sender@example.com"}}
	send := func(owner string, ids ...string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string][]string{"signerIds": ids})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/documents/document/send", strings.NewReader(string(body)))
		req.SetPathValue("id", "document")
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, owner))
		w := httptest.NewRecorder()
		h.Send(w, req)
		return w
	}
	for _, tc := range []struct {
		name, owner string
		ids         []string
		want        int
	}{
		{"empty", "owner", []string{}, 400},
		{"duplicate", "owner", []string{firstSigner, firstSigner}, 400},
		{"invalid or foreign signer", "owner", []string{firstSigner, missingSigner}, 400},
		{"missing assignment", "owner", []string{firstSigner, thirdSigner}, 400},
		{"wrong owner", "another-user", []string{firstSigner}, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := send(tc.owner, tc.ids...)
			if w.Code != tc.want || len(recipients) != 0 {
				t.Fatalf("status = %d, deliveries = %d", w.Code, len(recipients))
			}
		})
	}
	w := send("owner", secondSigner)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"sent"`) ||
		!reflect.DeepEqual(recipients, []string{"second@example.com"}) ||
		strings.Contains(w.Body.String(), "first-token") || db.targets[firstSigner].AttemptedAt != nil {
		t.Fatal("the first request must send only to the checked signer")
	}
	w = send("owner", firstSigner, secondSigner)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"already_sent"`) ||
		!reflect.DeepEqual(recipients, []string{"second@example.com", "first@example.com"}) {
		t.Fatal("the retry must suppress the accepted invitation and send the newly selected invitation")
	}
}
