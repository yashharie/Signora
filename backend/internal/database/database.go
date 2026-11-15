package database

import (
	"context"
	_ "embed"
	"log"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

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
	if pool == nil || schemaSQL == "" {
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
