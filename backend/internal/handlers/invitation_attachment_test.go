package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
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
	err := sendInvitationEmail(send, "signer@example.com", "Agreement", "signing link", "signing link", pdf, &result)
	if !errors.Is(err, services.ErrDeliveryUncertain) || !reflect.DeepEqual(attachmentCounts, []int{1}) || result.AttachmentError != "" {
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
	err := sendInvitationEmail(mailer.Send, "signer@example.com", "Agreement", link, link, pdf, &result)
	if err != nil || !reflect.DeepEqual(attachmentCounts, []int{1, 0}) || result.AttachmentError == "" {
		t.Fatalf("link-only fallback did not complete correctly: %v", err)
	}
}
