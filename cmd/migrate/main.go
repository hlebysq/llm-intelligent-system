package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"

	"github.com/hlebysq/llm-intelligent-system/internal/migrations"
)

func main() {
	dir := flag.String("dir", getEnv("MIGRATIONS_DIR", "migrations"), "directory with .sql migrations")
	timeout := flag.Duration("timeout", 2*time.Minute, "migration timeout")
	flag.Parse()

	db, err := sql.Open("postgres", postgresDSN())
	if err != nil {
		fatalf("open database: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		fatalf("ping database: %v", err)
	}

	applied, err := migrations.Up(ctx, db, *dir)
	if err != nil {
		fatalf("apply migrations: %v", err)
	}

	if len(applied) == 0 {
		fmt.Println("No pending migrations.")
		return
	}

	fmt.Println("Applied migrations:")
	for _, version := range applied {
		fmt.Printf("- %s\n", version)
	}
}

func postgresDSN() string {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}

	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		getEnv("DB_HOST", "localhost"),
		getEnv("DB_PORT", "5432"),
		getEnv("DB_USER", "llm_user"),
		getEnv("DB_PASSWORD", "password"),
		getEnv("DB_NAME", "llm_system"),
		getEnv("DB_SSLMODE", "disable"),
	)
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

