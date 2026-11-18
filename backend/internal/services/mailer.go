package services

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Mailer sends email through HTTPS APIs (Resend, Brevo) or standard SMTP servers.
type Mailer struct {
	Host, Port, User, Pass     string
	ResendKey, ResendFrom      string
	BrevoKey, BrevoSender      string
	DeliveryProvider, FromName string
	brevoClient                *http.Client
}

type Attachment struct {
	Filename string
	Path     string
}

func (m *Mailer) Provider() string {
	if m == nil {
		return "none"
	}
	if selected := strings.ToLower(strings.TrimSpace(m.DeliveryProvider)); selected != "" {
		switch selected {
		case "brevo", "smtp", "resend":
			return selected
		default:
			return "none"
		}
	}
	if strings.TrimSpace(m.ResendKey) != "" {
		return "resend"
	}
	if strings.TrimSpace(m.BrevoKey) != "" {
		return "brevo"
	}
	if strings.TrimSpace(m.User) != "" && strings.TrimSpace(m.Pass) != "" {
		return "smtp"
	}
	return "none"
}

func (m *Mailer) Configured() bool {
	if m == nil {
		return false
	}
	switch m.Provider() {
	case "brevo":
		return strings.TrimSpace(m.BrevoKey) != "" && (strings.TrimSpace(m.BrevoSender) != "" || strings.TrimSpace(m.User) != "")
	case "smtp":
		return strings.TrimSpace(m.User) != "" && strings.TrimSpace(m.Pass) != ""
	case "resend":
		return strings.TrimSpace(m.ResendKey) != ""
	}
	return false
}

// Send delivers one email. Returns an error (never panics) so callers can report per recipient.
func (m *Mailer) Send(to, subject, htmlBody, textBody string, attachments []Attachment) error {
	if !m.Configured() {
		err := errors.New("email is not configured: set RESEND_API_KEY, BREVO_API_KEY, or SMTP_USER/SMTP_PASS in Railway environment variables")
		log.Printf("[MAILER ERROR] %v", err)
		return err
	}

	// 1. Resend API (HTTPS port 443 - works everywhere, never blocked on Railway)
	if m.Provider() == "resend" {
		if err := m.sendViaResend(to, subject, htmlBody, textBody, attachments); err != nil {
			log.Printf("[MAILER RESEND ERROR] Delivery to %s failed: %v", to, err)
			return err
		}
		log.Printf("[MAILER RESEND SUCCESS] Email delivered to %s (subject: %q)", to, subject)
		return nil
	}

	// 2. Brevo API (HTTPS port 443 - works everywhere, never blocked on Railway)
	if m.Provider() == "brevo" {
		if err := m.sendViaBrevo(to, subject, htmlBody, textBody, attachments); err != nil {
			log.Printf("[MAILER BREVO ERROR] %v", err)
			return err
		}
		log.Print("[MAILER BREVO SUCCESS] Email accepted")
		return nil
	}

	// 3. SMTP (ports 587 / 465)
	msg, err := buildMessage(m.User, to, subject, htmlBody, textBody, attachments)
	if err != nil {
		log.Printf("[MAILER ERROR] Failed to build message for %s: %v", to, err)
		return err
	}
	if err := m.deliver(to, msg); err != nil {
		log.Printf("[MAILER ERROR] Delivery to %s failed: %v", to, err)
		return err
	}
	log.Printf("[MAILER SUCCESS] Email delivered to %s (subject: %q)", to, subject)
	return nil
}

func (m *Mailer) sendViaResend(to, subject, htmlBody, textBody string, atts []Attachment) error {
	from := strings.TrimSpace(m.ResendFrom)
	if from == "" {
		from = "Signora <onboarding@resend.dev>"
	}

	type resendAtt struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	var resendAtts []resendAtt
	for _, a := range atts {
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return fmt.Errorf("reading attachment %s: %w", a.Filename, err)
		}
		resendAtts = append(resendAtts, resendAtt{
			Filename: a.Filename,
			Content:  base64.StdEncoding.EncodeToString(data),
		})
	}

	payload := map[string]any{
		"from":    from,
		"to":      []string{to},
		"subject": subject,
		"html":    htmlBody,
		"text":    textBody,
	}
	if len(resendAtts) > 0 {
		payload["attachments"] = resendAtts
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://api.resend.com/emails", bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(m.ResendKey))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("resend HTTPS request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("resend error (%d): %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

var defaultBrevoClient = &http.Client{
	Timeout: 15 * time.Second,
	// Do not forward the API key to redirected endpoints.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (m *Mailer) senderName() string {
	if name := strings.TrimSpace(m.FromName); name != "" {
		return name
	}
	return "Signora"
}

func (m *Mailer) sendViaBrevo(to, subject, htmlBody, textBody string, atts []Attachment) error {
	senderEmail := strings.TrimSpace(m.BrevoSender)
	if senderEmail == "" {
		senderEmail = strings.TrimSpace(m.User)
	}
	if senderEmail == "" {
		return errors.New("brevo requires a sender email: set EMAIL_FROM (or legacy BREVO_SENDER / SMTP_USER) in Railway Variables")
	}

	type brevoAtt struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	var brevoAtts []brevoAtt
	for _, a := range atts {
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return fmt.Errorf("reading attachment %s: %w", a.Filename, err)
		}
		brevoAtts = append(brevoAtts, brevoAtt{
			Name:    a.Filename,
			Content: base64.StdEncoding.EncodeToString(data),
		})
	}

	payload := map[string]any{
		"sender":      map[string]string{"name": m.senderName(), "email": senderEmail},
		"to":          []map[string]string{{"email": to}},
		"subject":     subject,
		"htmlContent": htmlBody,
		"textContent": textBody,
	}
	if len(brevoAtts) > 0 {
		payload["attachment"] = brevoAtts
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://api.brevo.com/v3/smtp/email", bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("api-key", strings.TrimSpace(m.BrevoKey))
	req.Header.Set("Content-Type", "application/json")

	client := m.brevoClient
	if client == nil {
		client = defaultBrevoClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("brevo HTTPS request failed or timed out")
	}
	defer resp.Body.Close()

	// Never return provider response bodies: they can echo credentials or reset links.
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("brevo rejected email (HTTP %d)", resp.StatusCode)
	}
	if readErr != nil {
		return errors.New("brevo response could not be read")
	}
	return nil
}

func (m *Mailer) deliver(to string, msg []byte) error {
	// Try the configured port first
	err := m.deliverToPort(to, msg, m.Host, m.Port)
	if err == nil {
		return nil
	}

	log.Printf("[MAILER WARNING] Delivery on port %s failed: %v", m.Port, err)

	// Automatic fallback for Gmail / common SMTP servers between 587 and 465
	// Cloud providers often restrict port 587 but leave 465 open (or vice versa)
	fallbackPort := ""
	if m.Port == "587" {
		fallbackPort = "465"
	} else if m.Port == "465" {
		fallbackPort = "587"
	}

	if fallbackPort != "" {
		log.Printf("[MAILER] Attempting automatic fallback to port %s...", fallbackPort)
		fallbackErr := m.deliverToPort(to, msg, m.Host, fallbackPort)
		if fallbackErr == nil {
			log.Printf("[MAILER] Fallback to port %s succeeded!", fallbackPort)
			return nil
		}
		log.Printf("[MAILER WARNING] Fallback to port %s also failed: %v", fallbackPort, fallbackErr)
		return fmt.Errorf("mail server unreachable (tried ports %s: %v, %s: %v). Note: Railway blocks outbound SMTP ports 587/465 on Free/Hobby plans. To send emails on Railway, set RESEND_API_KEY (from resend.com) or BREVO_API_KEY (from brevo.com) in Railway Variables, or upgrade to Railway Pro.", m.Port, err, fallbackPort, fallbackErr)
	}

	return err
}

func (m *Mailer) deliverToPort(to string, msg []byte, host, port string) error {
	addr := net.JoinHostPort(host, port)
	var conn net.Conn
	var err error

	if port == "465" {
		// Port 465: Direct TLS connection (SMTPS)
		tlsDialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 15 * time.Second},
			Config:    &tls.Config{ServerName: host},
		}
		conn, err = tlsDialer.Dial("tcp", addr)
		if err != nil {
			return fmt.Errorf("tls connection failed on %s: %w", addr, err)
		}
	} else {
		// Port 587 / 25: Plain connection + STARTTLS upgrade
		conn, err = net.DialTimeout("tcp", addr, 15*time.Second)
		if err != nil {
			return fmt.Errorf("connection failed on %s: %w", addr, err)
		}
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp handshake error: %w", err)
	}
	defer c.Close()

	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return fmt.Errorf("STARTTLS error: %w", err)
			}
		}
	}

	auth := smtp.PlainAuth("", m.User, m.Pass, host)
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("mail login failed (verify your email and 16-character app password): %w", err)
	}

	if err := c.Mail(m.User); err != nil {
		return fmt.Errorf("MAIL FROM <%s> rejected: %w", m.User, err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO <%s> rejected: %w", to, err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA start failed: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("writing email body failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing data failed: %w", err)
	}

	return c.Quit()
}

var headerCleaner = strings.NewReplacer("\r", " ", "\n", " ")

func buildMessage(from, to, subject, htmlBody, textBody string, atts []Attachment) ([]byte, error) {
	var b bytes.Buffer
	mixed := "signora-mixed-" + randomHex()
	alt := "signora-alt-" + randomHex()

	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", "Signora <"+from+">")
	header("To", headerCleaner.Replace(to))
	header("Subject", mime.QEncoding.Encode("utf-8", headerCleaner.Replace(subject)))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", `multipart/mixed; boundary="`+mixed+`"`)
	b.WriteString("\r\n")

	b.WriteString("--" + mixed + "\r\n")
	b.WriteString(`Content-Type: multipart/alternative; boundary="` + alt + `"` + "\r\n\r\n")
	writeTextPart(&b, alt, "text/plain", textBody)
	writeTextPart(&b, alt, "text/html", htmlBody)
	b.WriteString("--" + alt + "--\r\n")

	for _, a := range atts {
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot read attachment %s: %w", a.Filename, err)
		}
		name := headerCleaner.Replace(a.Filename)
		ctype := mime.FormatMediaType("application/pdf", map[string]string{"name": name})
		disp := mime.FormatMediaType("attachment", map[string]string{"filename": name})
		if ctype == "" {
			ctype = "application/pdf"
		}
		if disp == "" {
			disp = "attachment"
		}
		b.WriteString("--" + mixed + "\r\n")
		b.WriteString("Content-Type: " + ctype + "\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		b.WriteString("Content-Disposition: " + disp + "\r\n\r\n")
		writeBase64(&b, data)
	}
	b.WriteString("--" + mixed + "--\r\n")
	return b.Bytes(), nil
}

func writeTextPart(b *bytes.Buffer, boundary, ctype, body string) {
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: " + ctype + "; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	writeBase64(b, []byte(body))
}

func writeBase64(b *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
}

func randomHex() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// ---------- Email templates (responsive: fluid 600px card, stacked button on phones) ----------

func emailLayout(inner string) string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>Signora</title>` +
		`</head><body style="margin:0;padding:24px;background:#f1f5f9;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;color:#0f172a;">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">` +
		`<table role="presentation" width="100%" style="max-width:560px;background:#ffffff;border-radius:12px;border:1px solid #e2e8f0;padding:32px;box-shadow:0 1px 3px rgba(0,0,0,0.04);" cellpadding="0" cellspacing="0">` +
		`<tr><td style="padding-bottom:20px;border-bottom:1px solid #f1f5f9;">` +
		`<span style="font-weight:700;font-size:20px;letter-spacing:-0.5px;color:#0f172a;">Signora</span>` +
		`</td></tr>` +
		inner +
		`<tr><td style="background:#f8fafc;padding:16px;text-align:center;color:#94a3b8;font-size:12px;">If you did not expect this email you can safely ignore it.</td></tr>` +
		`</table></td></tr></table></body></html>`
}

func SigningRequestEmail(signerName, senderName, docTitle, link string, expires time.Time, reminder bool) (subject, htmlBody, textBody string) {
	subject = fmt.Sprintf("Please sign: %s", docTitle)
	greeting := "Please review and sign this document"
	if reminder {
		subject = fmt.Sprintf("Reminder: please sign %s", docTitle)
		greeting = "Reminder: this document is still waiting for your signature"
	}
	inner := fmt.Sprintf(`<tr><td style="padding:24px 0 16px 0;">`+
		`<h1 style="font-size:18px;margin:0 0 12px 0;color:#0f172a;">%s</h1>`+
		`<p style="margin:0 0 16px 0;line-height:1.6;color:#334155;">Hello <strong>%s</strong>,<br><br><strong>%s</strong> has sent you <em>%s</em> for your electronic signature.</p>`+
		`<p style="margin:0 0 24px 0;line-height:1.6;color:#334155;">This link is unique to you and expires on <strong>%s</strong>.</p>`+
		`<a href="%s" style="display:inline-block;background:#0f172a;color:#ffffff;text-decoration:none;padding:12px 24px;border-radius:8px;font-weight:600;font-size:14px;">Review &amp; Sign Document</a>`+
		`<p style="margin:24px 0 0 0;font-size:12px;color:#94a3b8;word-break:break-all;">Or copy this link into your browser:<br>%s</p>`+
		`</td></tr>`,
		html.EscapeString(greeting), html.EscapeString(signerName), html.EscapeString(senderName),
		html.EscapeString(docTitle), expires.Format("January 2, 2006 at 3:04 PM MST"), link, link)
	textBody = fmt.Sprintf("Hello %s,\n\n%s has sent you \"%s\" for signature.\n\nPlease sign before %s:\n%s\n",
		signerName, senderName, docTitle, expires.Format("Jan 2, 2006 3:04 PM MST"), link)
	return subject, emailLayout(inner), textBody
}

func CompletionEmail(recipientName, docTitle string, attached bool) (subject, htmlBody, textBody string) {
	subject = fmt.Sprintf("Completed: %s", docTitle)
	note := "The fully signed PDF has been archived and is available on Signora."
	if attached {
		note = "The fully signed PDF, including a Certificate of Completion and audit trail, is attached to this email."
	}
	inner := fmt.Sprintf(`<tr><td style="padding:24px 0 16px 0;">`+
		`<h1 style="font-size:18px;margin:0 0 12px 0;color:#0f172a;">Document Signing Complete</h1>`+
		`<p style="margin:0 0 16px 0;line-height:1.6;color:#334155;">Hello <strong>%s</strong>,<br><br>All signers have signed <em>%s</em>.</p>`+
		`<p style="margin:0 0 24px 0;line-height:1.6;color:#334155;">%s</p>`+
		`</td></tr>`,
		html.EscapeString(recipientName), html.EscapeString(docTitle), html.EscapeString(note))
	textBody = fmt.Sprintf("Hello %s,\n\nAll signers have signed \"%s\".\n\n%s\n", recipientName, docTitle, note)
	return subject, emailLayout(inner), textBody
}
