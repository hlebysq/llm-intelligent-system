package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

type Config struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
}

type DB struct {
	*sql.DB
	logger *zap.Logger
}

// NewPostgresDB создает новое подключение к PostgreSQL
func NewPostgresDB(cfg Config, logger *zap.Logger) (*DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName,
	)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Настройка пула соединений
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Проверка соединения
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	logger.Info("Successfully connected to PostgreSQL",
		zap.String("host", cfg.Host),
		zap.String("database", cfg.DBName),
	)

	return &DB{
		DB:     db,
		logger: logger,
	}, nil
}

// Close закрывает соединение с БД
func (db *DB) Close() error {
	db.logger.Info("Closing database connection")
	return db.DB.Close()
}

// HealthCheck проверяет состояние БД
func (db *DB) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return db.PingContext(ctx)
}
