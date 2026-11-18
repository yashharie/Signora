package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"phase1-signora/backend/internal/services"
)

func TestPrepareInvitationPDF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "original.pdf")
	legacy := []byte("%PDF-1.7\noriginal disk contents")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	database := []byte("%PDF-1.7\noriginal database contents")
	for _, tc := range []struct {
		name           string
		data, want     []byte
		stored, report string
	}{
		{name: "database original preferred", data: database, want: database, stored: "original.pdf"},
		{name: "legacy disk original", stored: "original.pdf", want: legacy},
		{name: "missing", stored: "missing.pdf", report: invitationPDFUnavailable},
		{name: "traversal", stored: "../original.pdf", report: invitationPDFUnavailable},
		{name: "invalid PDF", data: []byte("not a PDF"), report: invitationPDFUnavailable},
		{name: "oversized database PDF", data: make([]byte, maxInvitationPDF+1), report: invitationPDFTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attachments, report := prepareInvitationPDF(tc.data, dir, tc.stored, "Invoice.PDF")
			if report != tc.report {
				t.Fatalf("attachment report %q, want %q", report, tc.report)
			}
			if tc.want == nil {
				if len(attachments) != 0 {
					t.Fatal("unexpected attachment")
				}
			} else if len(attachments) != 1 || attachments[0].Filename != "Invoice.PDF" || !bytes.Equal(attachments[0].Data, tc.want) {
				t.Fatal("attachment does not match the selected original")
			}
		})
	}
	if contents, err := os.ReadFile(path); err != nil || !bytes.Equal(contents, legacy) {
		t.Fatal("legacy original changed")
	}
}

func TestInvitationRetryEligibility(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		target sendTarget
		want   bool
	}{
		{"unsent or explicitly rejected", sendTarget{ExpiresAt: now.Add(time.Hour)}, true},
		{"in progress or uncertain", sendTarget{ExpiresAt: now.Add(time.Hour), AttemptedAt: &now}, false},
		{"already accepted", sendTarget{ExpiresAt: now.Add(time.Hour), EmailSent: true, AttemptedAt: &now}, false},
		{"expired accepted", sendTarget{ExpiresAt: now, EmailSent: true, AttemptedAt: &now}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.canClaim(now); got != tc.want {
				t.Fatalf("canClaim = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestInvitationDoesNotResendUncertainDelivery(t *testing.T) {
	pdf := []services.Attachment{{Filename: "original.pdf", Data: []byte("%PDF-1.7")}}
	var attachmentCounts []int
	send := func(to, subject, htmlBody, textBody string, attachments []services.Attachment) error {
		attachmentCounts = append(attachmentCounts, len(attachments))
		return services.ErrDeliveryUncertain
	}
	var result emailResult
	sendInvitationEmail(send, "signer@example.com", "Agreement", "signing link", "signing link", pdf, &result)
	if result.Outcome != invitationUnconfirmed || result.Success || !reflect.DeepEqual(attachmentCounts, []int{1}) || result.AttachmentError != "" {
		t.Fatal("uncertain send was retried or misclassified")
	}
}

type invitationTransport func(*http.Request) (*http.Response, error)

func (f invitationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestInvitationFallsBackToSameLinkAfterBrevoSizeRejection(t *testing.T) {
	const link = "https://signora.example/sign/test-link"
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var attachmentCounts []int
	http.DefaultTransport = invitationTransport(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			TextContent string
			Attachment  []json.RawMessage
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload.TextContent, link) {
			t.Fatal("secure signing link missing")
		}
		attachmentCounts = append(attachmentCounts, len(payload.Attachment))
		status := http.StatusCreated
		if len(attachmentCounts) == 1 {
			status = http.StatusRequestEntityTooLarge
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	mailer := &services.Mailer{DeliveryProvider: "brevo", BrevoKey: "test-key", BrevoSender: "sender@example.com"}
	pdf := []services.Attachment{{Filename: "original.pdf", Data: []byte("%PDF-1.7")}}
	var result emailResult
	sendInvitationEmail(mailer.Send, "signer@example.com", "Agreement", link, link, pdf, &result)
	if result.Outcome != invitationSentWithoutPDF || !result.Success || !reflect.DeepEqual(attachmentCounts, []int{1, 0}) || result.AttachmentError == "" {
		t.Fatalf("link-only fallback did not complete correctly: %+v", result)
	}
}

func TestInvitationDeliveryResults(t *testing.T) {
	const pdf = "%PDF-1.7 original"
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	mailer := &services.Mailer{DeliveryProvider: "brevo", BrevoKey: "test-key", BrevoSender: "sender@example.com"}
	for _, tc := range []struct {
		email, link string
		status      int
		want        invitationOutcome
	}{
		{"first@example.com", "https://signora.example/sign/first", http.StatusCreated, invitationSent},
		{"second@example.com", "https://signora.example/sign/second", http.StatusCreated, invitationSent},
		{"rejected@example.com", "https://signora.example/sign/rejected", http.StatusBadRequest, invitationFailed},
		{"uncertain@example.com", "https://signora.example/sign/uncertain", http.StatusBadGateway, invitationUnconfirmed},
	} {
		t.Run(string(tc.want)+"/"+tc.email, func(t *testing.T) {
			calls := 0
			http.DefaultTransport = invitationTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					To                       []struct{ Email string }
					HTMLContent, TextContent string
					Attachment               []struct {
						Name    string
						Content []byte
					}
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if req.URL.Host != "api.brevo.com" || len(payload.To) != 1 || payload.To[0].Email != tc.email ||
					!strings.Contains(payload.HTMLContent, tc.link) || !strings.Contains(payload.TextContent, tc.link) ||
					len(payload.Attachment) != 1 || payload.Attachment[0].Name != "original.pdf" || string(payload.Attachment[0].Content) != pdf {
					t.Fatal("expected only this signer, their link, and the original PDF")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("private provider payload"))}, nil
			})
			subject, htmlBody, textBody := services.SigningRequestEmail("Signer", "Sender", "Agreement", tc.link, time.Now(), false)
			attachments := []services.Attachment{{Filename: "original.pdf", Data: []byte(pdf)}}
			result := emailResult{Email: tc.email}
			sendInvitationEmail(mailer.Send, tc.email, subject, htmlBody, textBody, attachments, &result)
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != tc.want || result.Success != (tc.want == invitationSent) || calls != 1 ||
				!strings.Contains(string(data), `"outcome":"`+string(tc.want)+`"`) || strings.Contains(string(data), "private provider payload") {
				t.Fatalf("incorrect or unsafe invitation outcome: %s", data)
			}
		})
	}
}
