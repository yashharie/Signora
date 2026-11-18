package config

import "testing"

func TestEmailProviderEnvironment(t *testing.T) {
	t.Setenv("EMAIL_PROVIDER", " BREVO ")
	t.Setenv("BREVO_API_KEY", "test-key")
	t.Setenv("EMAIL_FROM", "verified@example.com")
	t.Setenv("EMAIL_FROM_NAME", "Signora Support")
	t.Setenv("BREVO_SENDER", "legacy@example.com")
	cfg := Load()
	if cfg.EmailProvider != "brevo" || cfg.BrevoKey != "test-key" || cfg.BrevoSender != "verified@example.com" || cfg.EmailFromName != "Signora Support" {
		t.Fatal("email configuration mapping failed")
	}
	t.Setenv("EMAIL_FROM", "")
	t.Setenv("EMAIL_FROM_NAME", "")
	cfg = Load()
	if cfg.BrevoSender != "legacy@example.com" || cfg.EmailFromName != "Signora" {
		t.Fatal("legacy settings not preserved")
	}
}
