package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type RedisClient struct {
	client *redis.Client
	logger *zap.Logger
}

type Config struct {
	Host     string
	Port     string
	Password string
	DB       int
}

// NewRedisClient создает нового Redis клиента
func NewRedisClient(cfg Config, logger *zap.Logger) (*RedisClient, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("%s:%s", cfg.Host, cfg.Port),
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     10,
	})

	// Проверка соединения
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	logger.Info("Successfully connected to Redis",
		zap.String("host", cfg.Host),
		zap.String("port", cfg.Port),
	)

	return &RedisClient{
		client: client,
		logger: logger,
	}, nil
}

// Get получает значение из кэша
func (r *RedisClient) Get(ctx context.Context, key string) (string, error) {
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("key not found: %s", key)
	}
	return val, err
}

// Set устанавливает значение в кэш с TTL
func (r *RedisClient) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

// Delete удаляет ключ из кэша
func (r *RedisClient) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

// Exists проверяет существование ключа
func (r *RedisClient) Exists(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	return n > 0, err
}

// Close закрывает соединение с Redis
func (r *RedisClient) Close() error {
	r.logger.Info("Closing Redis connection")
	return r.client.Close()
}

// HealthCheck проверяет состояние Redis
func (r *RedisClient) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return r.client.Ping(ctx).Err()
}
