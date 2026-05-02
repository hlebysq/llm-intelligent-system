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

// NewRedisClient creates a Redis client and verifies connectivity.
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	logger.Info("Successfully connected to Redis",
		zap.String("host", cfg.Host),
		zap.String("port", cfg.Port),
	)

	return &RedisClient{client: client, logger: logger}, nil
}

func (r *RedisClient) Get(ctx context.Context, key string) (string, error) {
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("key not found: %s", key)
	}
	return val, err
}

func (r *RedisClient) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *RedisClient) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

func (r *RedisClient) Exists(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	return n > 0, err
}

func (r *RedisClient) AllowRateLimit(ctx context.Context, key string, limit int, window time.Duration) (bool, int, time.Duration, error) {
	pipe := r.client.TxPipeline()
	countCmd := pipe.Incr(ctx, key)
	ttlCmd := pipe.TTL(ctx, key)

	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, 0, err
	}

	count := int(countCmd.Val())
	ttl := ttlCmd.Val()
	if count == 1 || ttl < 0 {
		if err := r.client.Expire(ctx, key, window).Err(); err != nil {
			return false, 0, 0, err
		}
		ttl = window
	}

	remaining := limit - count
	if remaining < 0 {
		remaining = 0
	}

	return count <= limit, remaining, ttl, nil
}

func NewTestClient(client *redis.Client) *RedisClient {
	return &RedisClient{client: client, logger: zap.NewNop()}
}

func (r *RedisClient) Close() error {
	r.logger.Info("Closing Redis connection")
	return r.client.Close()
}

func (r *RedisClient) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return r.client.Ping(ctx).Err()
}
