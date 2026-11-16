package config

import (
	"net"
	"os"
	"strings"
)

type Config struct {
	Port, DatabaseURL, JWTSecret, FrontendURL, UploadDir, SignedDir string
	SMTPHost, SMTPPort, SMTPUser, SMTPPass                          string
	ResendKey, ResendFrom                                           string
	BrevoKey, BrevoSender                                           string
}

func Load() Config {
	host := getFirst("SMTP_HOST", "MAIL_HOST", "EMAIL_HOST")
	if host == "" {
		host = "smtp.gmail.com"
	}
	port := getFirst("SMTP_PORT", "MAIL_PORT", "EMAIL_PORT")
	if port == "" {
		port = "587"
	}

	// If host was provided as "smtp.gmail.com:587", separate host and port
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		if port == "587" {
			port = p
		}
	}

	user := getFirst(
		"SMTP_USER",
		"SMTP_EMAIL",
		"EMAIL",
		"EMAIL_USER",
		"MAIL_USER",
		"MAIL_USERNAME",
		"SMTP_USERNAME",
		"GMAIL_USER",
		"GMAIL_EMAIL",
	)
	user = cleanUser(user)

	pass := getFirst(
		"SMTP_PASS",
		"SMTP_PASSWORD",
		"APP_PASSWORD",
		"GMAIL_APP_PASSWORD",
		"GMAIL_PASSWORD",
		"EMAIL_PASSWORD",
		"EMAIL_PASS",
		"MAIL_PASS",
		"MAIL_PASSWORD",
	)
	pass = cleanPass(host, pass)

	frontendURL := getFirst("FRONTEND_URL", "APP_URL", "VERCEL_URL")
	if frontendURL == "" {
		frontendURL = "http://localhost:3000"
	} else if !strings.HasPrefix(frontendURL, "http://") && !strings.HasPrefix(frontendURL, "https://") {
		frontendURL = "https://" + frontendURL
	}

	resendKey := getFirst("RESEND_API_KEY", "RESEND_KEY")
	resendFrom := getFirst("RESEND_FROM", "MAIL_FROM")
	brevoKey := getFirst("BREVO_API_KEY", "SENDINBLUE_API_KEY")
	brevoSender := getFirst("BREVO_SENDER", "BREVO_EMAIL")
	if brevoSender == "" {
		brevoSender = user
	}

	return Config{
		Port:        getFirstDefault("8080", "PORT"),
		DatabaseURL: getFirstDefault("postgres://postgres:postgres@localhost:5432/signora_db?sslmode=disable", "DATABASE_URL"),
		JWTSecret:   getFirstDefault("development_only_change_me", "JWT_SECRET"),
		FrontendURL: strings.TrimRight(frontendURL, "/"),
		UploadDir:   getFirstDefault("./uploads", "UPLOAD_DIR"),
		SignedDir:   getFirstDefault("./signed", "SIGNED_DIR"),
		SMTPHost:    strings.TrimSpace(host),
		SMTPPort:    strings.TrimSpace(port),
		SMTPUser:    user,
		SMTPPass:    pass,
		ResendKey:   resendKey,
		ResendFrom:  resendFrom,
		BrevoKey:    brevoKey,
		BrevoSender: brevoSender,
	}
}

func getFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func getFirstDefault(fallback string, keys ...string) string {
	if v := getFirst(keys...); v != "" {
		return v
	}
	return fallback
}

func cleanUser(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "\"'")
	return v
}

func cleanPass(host, v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "\"'")
	// If it's Gmail or a 16-character code with spaces (Google app passwords look like "abcd efgh ijkl mnop")
	if strings.Contains(host, "gmail") || strings.Contains(host, "google") {
		v = strings.ReplaceAll(v, " ", "")
	} else if len(strings.ReplaceAll(v, " ", "")) == 16 {
		v = strings.ReplaceAll(v, " ", "")
	}
	return v
}
