package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mailTransport func(*http.Request) (*http.Response, error)

func (f mailTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testBrevo(fn mailTransport) *Mailer {
	return &Mailer{DeliveryProvider: "brevo", BrevoKey: "test-secret-do-not-log", BrevoSender: "sender@example.com", FromName: "Signora Test", ResendKey: "other-provider", brevoClient: &http.Client{Transport: fn, Timeout: time.Second}}
}
func mailResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestBrevoPayloadAndRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signed.pdf")
	if err := os.WriteFile(path, []byte("PDF content"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := testBrevo(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != "https://api.brevo.com/v3/smtp/email" {
			t.Fatalf("wrong request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("api-key") != "test-secret-do-not-log" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("incorrect authentication or content type")
		}
		var payload struct {
			Sender                            map[string]string
			To                                []map[string]string
			Subject, HTMLContent, TextContent string
			Attachment                        []map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Sender["email"] != "sender@example.com" || payload.Sender["name"] != "Signora Test" || len(payload.To) != 1 || payload.To[0]["email"] != "registered@example.com" {
			t.Fatalf("wrong sender/recipient: %+v", payload)
		}
		if payload.Subject != "Reset password" || payload.HTMLContent != "<p>Reset link</p>" || payload.TextContent != "Reset link" {
			t.Fatal("message content changed")
		}
		if len(payload.Attachment) != 1 || payload.Attachment[0]["content"] != base64.StdEncoding.EncodeToString([]byte("PDF content")) || payload.Attachment[0]["name"] != "signed.pdf" {
			t.Fatal("attachment changed")
		}
		return mailResponse(201, `{"messageId":"test"}`), nil
	})
	if err := m.Send("registered@example.com", "Reset password", "<p>Reset link</p>", "Reset link", []Attachment{{Filename: "signed.pdf", Path: path}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestBrevoSafeErrors(t *testing.T) {
	for _, status := range []int{301, 400, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			m := testBrevo(func(*http.Request) (*http.Response, error) {
				return mailResponse(status, "test-secret-do-not-log reset-token"), nil
			})
			err := m.Send("registered@example.com", "Reset", "html", "text", nil)
			if err == nil {
				t.Fatal("expected rejection")
			}
			for _, secret := range []string{"test-secret-do-not-log", "reset-token"} {
				if strings.Contains(err.Error()+logs.String(), secret) {
					t.Fatal("secret leaked")
				}
			}
		})
	}
	m := testBrevo(func(*http.Request) (*http.Response, error) { return nil, errors.New("test-secret-do-not-log") })
	if err := m.Send("registered@example.com", "Reset", "html", "text", nil); err == nil || strings.Contains(err.Error(), m.BrevoKey) {
		t.Fatal("unsafe transport error")
	}
}

func TestBrevoSigningInvitation(t *testing.T) {
	const pdf = "%PDF-1.7\nunchanged original"
	const link = "https://signora.example/sign/test-signer-token"
	const title = "Supplier Agreement"
	calls := 0
	m := testBrevo(func(r *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			To          []map[string]string
			Subject     string
			HTMLContent string
			TextContent string
			Attachment  []struct{ Name, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.To) != 1 || payload.To[0]["email"] != "signer@example.com" ||
			!strings.Contains(payload.Subject, title) || !strings.Contains(payload.HTMLContent, "Sign Document") ||
			!strings.Contains(payload.HTMLContent, link) || !strings.Contains(payload.TextContent, link) {
			t.Fatal("invitation recipient or content is incorrect")
		}
		if calls == 1 {
			if len(payload.Attachment) != 1 || payload.Attachment[0].Name != "agreement.pdf" ||
				payload.Attachment[0].Content != base64.StdEncoding.EncodeToString([]byte(pdf)) {
				t.Fatal("original PDF attachment missing or changed")
			}
			return mailResponse(http.StatusRequestEntityTooLarge, "ignored provider body"), nil
		}
		if len(payload.Attachment) != 0 {
			t.Fatal("link-only email unexpectedly contains an attachment")
		}
		return mailResponse(http.StatusCreated, "{}"), nil
	})
	subject, htmlBody, textBody := SigningRequestEmail("Signer", "Sender", title, link, time.Now().Add(time.Hour), false)
	attachments := []Attachment{{Filename: "agreement.pdf", Data: []byte(pdf)}}
	if err := m.Send("signer@example.com", subject, htmlBody, textBody, attachments); !IsAttachmentSizeRejection(err) || errors.Is(err, ErrDeliveryUncertain) {
		t.Fatalf("size rejection was misclassified: %v", err)
	}
	if err := m.Send("signer@example.com", subject, htmlBody, textBody, nil); err != nil || calls != 2 {
		t.Fatalf("link-only delivery failed: %v", err)
	}
}

func TestBrevoTimeoutAndRedirectPolicy(t *testing.T) {
	m := testBrevo(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	m.brevoClient.Timeout = 10 * time.Millisecond
	start := time.Now()
	if err := m.Send("registered@example.com", "Reset", "html", "text", nil); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout was not bounded")
	}
	if defaultBrevoClient.Timeout != 15*time.Second {
		t.Fatal("missing production timeout")
	}
	if err := defaultBrevoClient.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirect could expose key")
	}
}

func TestProviderSelection(t *testing.T) {
	for _, tc := range []struct {
		selection, want string
		configured      bool
	}{
		{"brevo", "brevo", true}, {"smtp", "smtp", true}, {"resend", "resend", true}, {"", "resend", true}, {" SMTP ", "smtp", true}, {"invalid", "none", false},
	} {
		m := &Mailer{DeliveryProvider: tc.selection, User: "sender@example.com", Pass: "smtp-password", BrevoKey: "key", BrevoSender: "sender@example.com", ResendKey: "key"}
		if m.Provider() != tc.want || m.Configured() != tc.configured {
			t.Fatalf("selection %q: %s %v", tc.selection, m.Provider(), m.Configured())
		}
	}
	m := testBrevo(func(*http.Request) (*http.Response, error) {
		t.Fatal("must not send with missing key")
		return nil, nil
	})
	m.BrevoKey = ""
	if m.Configured() || m.Send("recipient@example.com", "subject", "html", "text", nil) == nil {
		t.Fatal("missing credentials accepted")
	}
	m.DeliveryProvider = "smtp"
	m.User = ""
	m.Pass = ""
	if m.Configured() {
		t.Fatal("explicit smtp must not fall back to API")
	}
}

func TestBrevoRedirectIsNotFollowed(t *testing.T) {
	calls := 0
	m := testBrevo(nil)
	client := *defaultBrevoClient
	client.Transport = mailTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.brevo.com" {
			t.Fatal("credentials forwarded to another host")
		}
		response := mailResponse(307, "redirect")
		response.Header.Set("Location", "https://another-host.example/email")
		return response, nil
	})
	m.brevoClient = &client
	if err := m.Send("registered@example.com", "Reset", "html", "text", nil); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls != 1 {
		t.Fatalf("unexpected calls: %d", calls)
	}
}

func TestSMTPDispatchRemainsSeparate(t *testing.T) {
	m := testBrevo(func(*http.Request) (*http.Response, error) {
		t.Fatal("SMTP selection must not call Brevo")
		return nil, nil
	})
	m.DeliveryProvider = "smtp"
	m.User = "smtp@example.com"
	m.Pass = "smtp-password"
	// The original MIME builder must process SMTP attachments before delivery.
	// A missing file prevents any real SMTP connection in this test.
	err := m.Send("recipient@example.com", "subject", "html", "text", []Attachment{{Filename: "missing.pdf", Path: filepath.Join(t.TempDir(), "missing.pdf")}})
	if err == nil || !strings.Contains(err.Error(), "cannot read attachment") {
		t.Fatalf("did not use original SMTP builder: %v", err)
	}
}

func TestBrevoResetWithoutAttachmentsAndLegacySender(t *testing.T) {
	m := testBrevo(func(r *http.Request) (*http.Response, error) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["attachment"]; exists {
			t.Fatal("reset email must not contain attachments")
		}
		var sender map[string]string
		_ = json.Unmarshal(payload["sender"], &sender)
		if sender["email"] != "legacy@example.com" || sender["name"] != "Signora" {
			t.Fatal("legacy sender fallback changed")
		}
		return mailResponse(201, `{}`), nil
	})
	m.BrevoSender = ""
	m.User = "legacy@example.com"
	m.FromName = ""
	if err := m.Send("registered@example.com", "Reset password", "html", "text", nil); err != nil {
		t.Fatal(err)
	}
	m.User = ""
	if m.Configured() {
		t.Fatal("missing sender accepted")
	}
}
