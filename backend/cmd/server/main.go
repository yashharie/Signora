package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"phase1-signora/backend/internal/config"
	"phase1-signora/backend/internal/database"
	"phase1-signora/backend/internal/handlers"
	"phase1-signora/backend/internal/middleware"
	"phase1-signora/backend/internal/services"
)

func main() {
	loadEnvFile()
	cfg := config.Load()
	db := database.Connect(cfg.DatabaseURL)
	defer db.Close()

	if err := os.MkdirAll(cfg.UploadDir, 0755); err != nil {
		log.Printf("WARNING: MkdirAll UploadDir: %v", err)
	}
	if err := os.MkdirAll(cfg.SignedDir, 0755); err != nil {
		log.Printf("WARNING: MkdirAll SignedDir: %v", err)
	}

	mailer := &services.Mailer{
		Host:        cfg.SMTPHost,
		Port:        cfg.SMTPPort,
		User:        cfg.SMTPUser,
		Pass:        cfg.SMTPPass,
		ResendKey:   cfg.ResendKey,
		ResendFrom:  cfg.ResendFrom,
		BrevoKey:    cfg.BrevoKey,
		BrevoSender: cfg.BrevoSender,
	}
	if !mailer.Configured() {
		log.Println("WARNING: No email provider configured (set RESEND_API_KEY, BREVO_API_KEY, or SMTP_USER/SMTP_PASS in Railway Variables).")
	} else {
		log.Printf("Mailer configured with provider: %s", mailer.Provider())
	}

	auth := handlers.AuthHandler{DB: db, Secret: cfg.JWTSecret, FrontendURL: cfg.FrontendURL, Mailer: mailer}
	docs := handlers.DocumentHandler{DB: db, UploadDir: cfg.UploadDir}
	signers := handlers.SignersHandler{DB: db, FrontendURL: cfg.FrontendURL, Mailer: mailer}
	boxes := handlers.BoxesHandler{DB: db}
	status := handlers.StatusHandler{DB: db, UploadDir: cfg.UploadDir, SignedDir: cfg.SignedDir, Mailer: mailer}
	signing := handlers.SigningHandler{DB: db, UploadDir: cfg.UploadDir, SignedDir: cfg.SignedDir, Mailer: mailer}

	mux := http.NewServeMux()

	// Health check - always returns 200 so Railway deployment health check passes
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		dbStatus := "connected"
		if db == nil || db.Ping(pingCtx) != nil {
			dbStatus = "connecting_or_unavailable"
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"database": dbStatus,
			"service":  "signora-backend",
			"mailer": map[string]any{
				"configured": mailer.Configured(),
				"provider":   mailer.Provider(),
				"user":       maskEmail(cfg.SMTPUser),
				"host":       cfg.SMTPHost,
				"port":       cfg.SMTPPort,
			},
		})
	})

	// Diagnostic endpoint to test email delivery
	mux.HandleFunc("GET /api/test-email", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		to := strings.TrimSpace(r.URL.Query().Get("to"))
		if to == "" {
			to = cfg.SMTPUser
		}
		if to == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "Recipient email is required (pass ?to=user@example.com) and SMTP_USER is not configured.",
				"mailer": map[string]any{
					"configured": mailer.Configured(),
					"provider":   mailer.Provider(),
					"user":       maskEmail(cfg.SMTPUser),
					"host":       cfg.SMTPHost,
					"port":       cfg.SMTPPort,
				},
			})
			return
		}

		err := mailer.Send(to, "Signora Email Test", "<p>Your Signora email service is configured and working!</p>", "Your Signora email service is configured and working!", nil)
		if err != nil {
			log.Printf("[TEST-EMAIL ERROR] %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   err.Error(),
				"to":      to,
				"mailer": map[string]any{
					"configured": mailer.Configured(),
					"provider":   mailer.Provider(),
					"user":       maskEmail(cfg.SMTPUser),
					"host":       cfg.SMTPHost,
					"port":       cfg.SMTPPort,
				},
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"message": "Test email sent successfully to " + to,
			"mailer": map[string]any{
				"configured": true,
				"provider":   mailer.Provider(),
				"user":       maskEmail(cfg.SMTPUser),
				"host":       cfg.SMTPHost,
				"port":       cfg.SMTPPort,
			},
		})
	})

	// Public Auth endpoints
	mux.HandleFunc("POST /api/auth/register", auth.Register)
	mux.HandleFunc("POST /api/auth/login", auth.Login)
	mux.HandleFunc("POST /api/auth/forgot-password", auth.ForgotPassword)
	mux.HandleFunc("POST /api/auth/reset-password", auth.ResetPassword)

	// Public Signing endpoints (token-based access)
	mux.HandleFunc("GET /api/signing/{token}", signing.GetSigningInfo)
	mux.HandleFunc("GET /api/signing/{token}/pdf", signing.ServeSigningPDF)
	mux.HandleFunc("POST /api/signing/{token}/submit", signing.SubmitSignature)

	// Protected endpoints (require user JWT)
	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/auth/me", auth.Me)

	// Documents
	protected.HandleFunc("POST /api/documents/upload", docs.Upload)
	protected.HandleFunc("GET /api/documents", docs.List)
	protected.HandleFunc("GET /api/documents/{id}/view", docs.View)
	protected.HandleFunc("GET /api/documents/{id}/download", docs.Download)
	protected.HandleFunc("DELETE /api/documents/{id}", docs.Delete)

	// Signers
	protected.HandleFunc("GET /api/documents/{id}/signers", signers.List)
	protected.HandleFunc("POST /api/documents/{id}/signers", signers.Add)
	protected.HandleFunc("DELETE /api/documents/{id}/signers/{signerId}", signers.Delete)
	protected.HandleFunc("POST /api/documents/{id}/send", signers.Send)

	// Signature Boxes
	protected.HandleFunc("GET /api/documents/{id}/boxes", boxes.List)
	protected.HandleFunc("POST /api/documents/{id}/boxes", boxes.Save)

	// Status, Audit Trail & Completed Download
	protected.HandleFunc("GET /api/documents/{id}/status", status.GetStatus)
	protected.HandleFunc("GET /api/documents/{id}/audit", status.GetAuditLogs)
	protected.HandleFunc("GET /api/documents/{id}/signed-download", status.DownloadSigned)
	protected.HandleFunc("POST /api/documents/{id}/regenerate", status.Regenerate)

	// Attach JWT middleware to protected routes
	mux.Handle("/api/auth/me", middleware.Auth(cfg.JWTSecret)(protected))
	mux.Handle("/api/documents", middleware.Auth(cfg.JWTSecret)(protected))
	mux.Handle("/api/documents/", middleware.Auth(cfg.JWTSecret)(protected))

	handler := cors(cfg.FrontendURL, mux)
	log.Printf("Signora Go API running on :%s", cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, handler))
}

func cors(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOrigin := r.Header.Get("Origin")
		if requestOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", requestOrigin)
		} else if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Origin, X-Requested-With, Cache-Control")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS, HEAD")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loadEnvFile() {
	for _, path := range []string{".env", "backend/.env"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
				if os.Getenv(key) == "" {
					_ = os.Setenv(key, value)
				}
			}
		}
		break
	}
}

func maskEmail(email string) string {
	if email == "" {
		return "[not configured]"
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "[configured]"
	}
	u := parts[0]
	if len(u) <= 2 {
		return u + "***@" + parts[1]
	}
	return u[:2] + "***@" + parts[1]
}
