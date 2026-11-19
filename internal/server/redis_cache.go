package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	overviewCacheTTL = 5 * time.Second
	storageCacheTTL  = 10 * time.Second
	cacheTimeout     = 120 * time.Millisecond
	maxCachePayload  = 1 << 20
)

var redisNamespace = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type adminRedisCache struct {
	client *redis.Client
	prefix string
}

func openAdminRedisCache(endpoint, namespace, driver string) (*adminRedisCache, error) {
	if endpoint == "" {
		return nil, nil
	}
	if !redisNamespace.MatchString(namespace) {
		return nil, errors.New("set HARBOR_REDIS_NAMESPACE to a unique deployment name (1–64 letters, digits, hyphens or underscores)")
	}
	options, err := redis.ParseURL(endpoint)
	if err != nil || (options.TLSConfig != nil && options.TLSConfig.InsecureSkipVerify) {
		return nil, errors.New("HARBOR_REDIS_URL must contain a valid Redis connection URL")
	}
	options.DialTimeout = cacheTimeout
	options.ReadTimeout = cacheTimeout
	options.WriteTimeout = cacheTimeout
	options.PoolTimeout = cacheTimeout
	options.PoolSize = 4
	options.MinIdleConns = 0
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	return &adminRedisCache{
		client: redis.NewClient(options),
		prefix: "harbor-mail:" + namespace + ":" + driver + ":admin:v1:",
	}, nil
}

func (c *adminRedisCache) read(ctx context.Context, key string, value any) (hit, reachable bool) {
	if c == nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(ctx, cacheTimeout)
	defer cancel()
	data, err := c.client.Get(ctx, c.prefix+key).Bytes()
	return err == nil && len(data) <= maxCachePayload && json.Unmarshal(data, value) == nil,
		err == nil || errors.Is(err, redis.Nil)
}

func (c *adminRedisCache) write(ctx context.Context, key string, value any, ttl time.Duration) {
	if c == nil {
		return
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxCachePayload {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cacheTimeout)
	defer cancel()
	_ = c.client.Set(ctx, c.prefix+key, data, ttl).Err()
}

func storageCacheKey(owner string) string {
	hash := sha256.Sum256([]byte(owner))
	return "storage:" + hex.EncodeToString(hash[:])
}
