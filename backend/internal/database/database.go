package database

import (
	"context"
	"log"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name VARCHAR(120) NOT NULL,
  email VARCHAR(255) NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS documents (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title VARCHAR(200) NOT NULL,
  original_name TEXT NOT NULL,
  stored_name TEXT NOT NULL UNIQUE,
  signed_file_name TEXT,
  file_size BIGINT NOT NULL,
  status VARCHAR(20) NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','pending','completed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_documents_user_created ON documents(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS signers (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  document_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  name VARCHAR(120) NOT NULL,
  email VARCHAR(255) NOT NULL,
  token VARCHAR(64) UNIQUE NOT NULL,
  token_expires_at TIMESTAMPTZ NOT NULL,
  status VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'signed')),
  signed_at TIMESTAMPTZ,
  signature_data TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_document_signer_email UNIQUE (document_id, email)
);

CREATE INDEX IF NOT EXISTS idx_signers_token ON signers(token);
CREATE INDEX IF NOT EXISTS idx_signers_document_id ON signers(document_id);

CREATE TABLE IF NOT EXISTS signature_boxes (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  document_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  signer_id UUID REFERENCES signers(id) ON DELETE SET NULL,
  page INT NOT NULL DEFAULT 1,
  x DOUBLE PRECISION NOT NULL,
  y DOUBLE PRECISION NOT NULL,
  width DOUBLE PRECISION NOT NULL,
  height DOUBLE PRECISION NOT NULL,
  signed BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_signature_boxes_document_id ON signature_boxes(document_id);
CREATE INDEX IF NOT EXISTS idx_signature_boxes_signer_id ON signature_boxes(signer_id);

CREATE TABLE IF NOT EXISTS audit_logs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  document_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  action VARCHAR(50) NOT NULL,
  performed_by VARCHAR(255) NOT NULL,
  details TEXT,
  ip_address VARCHAR(45),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_document_created ON audit_logs(document_id, created_at ASC);

ALTER TABLE documents ADD COLUMN IF NOT EXISTS signed_file_name TEXT;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS file_data BYTEA;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS signed_file_data BYTEA;
ALTER TABLE signers   ADD COLUMN IF NOT EXISTS email_sent BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE signers   ADD COLUMN IF NOT EXISTS email_sent_at TIMESTAMPTZ;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS completion_emailed_at TIMESTAMPTZ;
`

func Connect(databaseURL string) *pgxpool.Pool {
	ctx := context.Background()
	log.Printf("Initializing database connection to: %s", MaskDatabaseURL(databaseURL))

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Printf("WARNING: Could not parse DATABASE_URL (%v). Using default fallback config.", err)
		cfg, _ = pgxpool.ParseConfig("postgres://postgres:postgres@localhost:5432/signora_db?sslmode=disable")
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Printf("ERROR: Failed to allocate connection pool: %v", err)
	}

	// Fast initial ping attempts (e.g. 3 attempts, 1s apart)
	connected := false
	for attempt := 1; attempt <= 3; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if pool != nil && pool.Ping(pingCtx) == nil {
			cancel()
			log.Printf("Database connected successfully on attempt %d.", attempt)
			connected = true
			ensureSchema(pool)
			break
		}
		cancel()
		if attempt < 3 {
			time.Sleep(1 * time.Second)
		}
	}

	// If not connected yet (e.g. Neon cold boot), retry in background so HTTP server can start
	if !connected {
		log.Println("Database not ready yet. Retrying in background while HTTP server starts...")
		go func() {
			for attempt := 4; attempt <= 15; attempt++ {
				time.Sleep(3 * time.Second)
				pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if pool != nil && pool.Ping(pingCtx) == nil {
					cancel()
					log.Printf("Database connected in background on attempt %d.", attempt)
					ensureSchema(pool)
					return
				}
				cancel()
				log.Printf("Background database connection attempt %d/15 failed.", attempt)
			}
			log.Println("WARNING: Database connection could not be established after all retries.")
		}()
	}

	return pool
}

func ensureSchema(pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		log.Printf("WARNING: Schema initialization reported error: %v", err)
	} else {
		log.Println("Database schema verified and up to date.")
	}
}

func MaskDatabaseURL(raw string) string {
	if raw == "" {
		return "[empty]"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable URL]"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "********")
		}
	}
	return u.String()
}
