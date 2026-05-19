package clientauth

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"

	"aevons-gateway/internal/model"

	frameworkredis "github.com/calmlax/aevons-framework/redis"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

type dbOauthClient struct {
	ClientID  string `gorm:"column:client_id"`
	Resources string `gorm:"column:resources"`
}

func (dbOauthClient) TableName() string {
	return "sys_oauth_client"
}

type Checker struct {
	enabled        bool
	db             *gorm.DB
	refreshEvery   time.Duration
	cacheKey       string
	backupCacheKey string
	lockKey        string
	cacheTTL       time.Duration
	backupTTL      time.Duration
	lockTTL        time.Duration
	jitterMax      time.Duration

	mu          sync.RWMutex
	cachedRules map[string]model.ClientRule
	lastLoaded  time.Time
	sf          singleflight.Group
}

func NewChecker(enabled bool, db *gorm.DB, refreshEvery time.Duration) *Checker {
	if refreshEvery <= 0 {
		refreshEvery = 30 * time.Second
	}
	return &Checker{
		enabled:        enabled,
		db:             db,
		refreshEvery:   refreshEvery,
		cacheKey:       "gateway:oauth-client-rules:v1",
		backupCacheKey: "gateway:oauth-client-rules:backup:v1",
		lockKey:        "gateway:oauth-client-rules:lock:v1",
		cacheTTL:       refreshEvery,
		backupTTL:      refreshEvery * 5,
		lockTTL:        5 * time.Second,
		jitterMax:      maxDuration(refreshEvery/3, 5*time.Second),
		cachedRules:    map[string]model.ClientRule{},
	}
}

func (c *Checker) Allow(clientID string, service *model.ServiceRule, path string) bool {
	if c == nil || !c.enabled {
		return true
	}
	rule, ok := c.Rule(clientID)
	if !ok || !rule.Enabled {
		return false
	}
	if rule.AllowAll {
		return true
	}
	path = strings.TrimSpace(path)
	if path != "" {
		if _, ok = rule.ExactRules[path]; ok {
			return true
		}
		for _, prefix := range rule.PrefixRules {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
	}
	if service == nil {
		return false
	}
	_, ok = rule.ServiceNames[service.Name]
	if ok {
		return true
	}
	_, ok = rule.ServiceNames[service.ID]
	return ok
}

func (c *Checker) Rule(clientID string) (model.ClientRule, bool) {
	if c == nil || !c.enabled {
		return model.ClientRule{}, false
	}
	_ = c.ensureLoaded(context.Background())

	c.mu.RLock()
	defer c.mu.RUnlock()
	rule, ok := c.cachedRules[strings.TrimSpace(clientID)]
	return rule, ok
}

func (c *Checker) Refresh(ctx context.Context) (int, error) {
	if c == nil || !c.enabled {
		return 0, nil
	}

	dbRules, err := c.loadFromDB(ctx)
	if err != nil {
		return 0, err
	}
	if err := c.storeToRedis(ctx, dbRules); err != nil {
		return 0, err
	}

	c.mu.Lock()
	c.cachedRules = cloneRules(dbRules)
	c.lastLoaded = time.Now()
	c.mu.Unlock()
	return len(dbRules), nil
}

func (c *Checker) ensureLoaded(ctx context.Context) error {
	c.mu.RLock()
	if time.Since(c.lastLoaded) < c.refreshEvery && len(c.cachedRules) > 0 {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	_, err, _ := c.sf.Do("gateway-oauth-client-rules", func() (any, error) {
		c.mu.RLock()
		if time.Since(c.lastLoaded) < c.refreshEvery && len(c.cachedRules) > 0 {
			c.mu.RUnlock()
			return nil, nil
		}
		c.mu.RUnlock()

		c.mu.Lock()
		defer c.mu.Unlock()
		if time.Since(c.lastLoaded) < c.refreshEvery && len(c.cachedRules) > 0 {
			return nil, nil
		}

		if redisRules, err := c.loadFromRedis(ctx); err == nil && len(redisRules) > 0 {
			c.cachedRules = cloneRules(redisRules)
			c.lastLoaded = time.Now()
			return nil, nil
		}

		dbRules, err := c.loadThroughLock(ctx)
		if err == nil && len(dbRules) > 0 {
			c.cachedRules = cloneRules(dbRules)
			c.lastLoaded = time.Now()
			return nil, nil
		}

		if len(c.cachedRules) > 0 {
			c.lastLoaded = time.Now()
			return nil, nil
		}

		c.cachedRules = map[string]model.ClientRule{}
		c.lastLoaded = time.Now()
		return nil, err
	})
	return err
}

func (c *Checker) loadFromDB(ctx context.Context) (map[string]model.ClientRule, error) {
	if c.db == nil {
		return nil, nil
	}

	var rows []dbOauthClient
	if err := c.db.WithContext(ctx).Model(&dbOauthClient{}).Find(&rows).Error; err != nil {
		return nil, err
	}

	configs := make([]model.ClientRuleConfig, 0, len(rows))
	for _, row := range rows {
		configs = append(configs, model.ClientRuleConfig{
			ClientID:  row.ClientID,
			Enabled:   true,
			Resources: splitResources(row.Resources),
		})
	}
	return buildRules(configs), nil
}

func (c *Checker) loadFromRedis(ctx context.Context) (map[string]model.ClientRule, error) {
	var rules map[string]model.ClientRule
	if err := frameworkredis.GetJSON(ctx, c.cacheKey, &rules); err != nil {
		var backup map[string]model.ClientRule
		if backupErr := frameworkredis.GetJSON(ctx, c.backupCacheKey, &backup); backupErr == nil && len(backup) > 0 {
			return backup, nil
		}
		return nil, err
	}
	if len(rules) == 0 {
		return nil, errors.New("gateway oauth client cache is empty")
	}
	return rules, nil
}

func (c *Checker) storeToRedis(ctx context.Context, rules map[string]model.ClientRule) error {
	if len(rules) == 0 {
		return nil
	}
	activeTTL := withJitter(c.cacheTTL, c.jitterMax)
	backupTTL := withJitter(c.backupTTL, c.jitterMax)
	if err := frameworkredis.SetJSON(ctx, c.cacheKey, rules, activeTTL); err != nil {
		return err
	}
	return frameworkredis.SetJSON(ctx, c.backupCacheKey, rules, backupTTL)
}

func (c *Checker) loadThroughLock(ctx context.Context) (map[string]model.ClientRule, error) {
	if c.db == nil {
		return nil, nil
	}

	raw, err := frameworkredis.Raw()
	if err != nil || raw == nil {
		dbRules, dbErr := c.loadFromDB(ctx)
		if dbErr == nil && len(dbRules) > 0 {
			_ = c.storeToRedis(ctx, dbRules)
		}
		return dbRules, dbErr
	}

	locked, err := raw.SetNX(ctx, c.lockKey, "1", c.lockTTL).Result()
	if err != nil {
		dbRules, dbErr := c.loadFromDB(ctx)
		if dbErr == nil && len(dbRules) > 0 {
			_ = c.storeToRedis(ctx, dbRules)
		}
		return dbRules, dbErr
	}

	if locked {
		defer raw.Del(context.Background(), c.lockKey)
		dbRules, dbErr := c.loadFromDB(ctx)
		if dbErr == nil && len(dbRules) > 0 {
			_ = c.storeToRedis(ctx, dbRules)
		}
		return dbRules, dbErr
	}

	return c.waitForWarmCache(ctx, raw)
}

func (c *Checker) waitForWarmCache(ctx context.Context, raw *goredis.Client) (map[string]model.ClientRule, error) {
	var lastErr error
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}

		rules, err := c.loadFromRedis(ctx)
		if err == nil && len(rules) > 0 {
			return rules, nil
		}
		lastErr = err

		exists, err := raw.Exists(ctx, c.lockKey).Result()
		if err == nil && exists == 0 {
			break
		}
	}

	dbRules, dbErr := c.loadFromDB(ctx)
	if dbErr == nil && len(dbRules) > 0 {
		_ = c.storeToRedis(ctx, dbRules)
		return dbRules, nil
	}
	if dbErr != nil {
		return nil, dbErr
	}
	return nil, lastErr
}

func buildRules(configs []model.ClientRuleConfig) map[string]model.ClientRule {
	rules := make(map[string]model.ClientRule, len(configs))
	for _, cfg := range configs {
		clientID := strings.TrimSpace(cfg.ClientID)
		if clientID == "" {
			continue
		}

		rule := model.ClientRule{
			ClientID:     clientID,
			Enabled:      cfg.Enabled,
			ServiceNames: map[string]struct{}{},
			ExactRules:   map[string]struct{}{},
			PrefixRules:  []string{},
		}

		for _, resource := range cfg.Resources {
			resource = strings.TrimSpace(resource)
			if resource == "" {
				continue
			}
			if strings.EqualFold(resource, "ALL") {
				rule.AllowAll = true
				continue
			}
			if strings.HasPrefix(resource, "/") {
				if strings.HasSuffix(resource, "/**") {
					rule.PrefixRules = append(rule.PrefixRules, strings.TrimSuffix(resource, "**"))
					continue
				}
				if strings.HasSuffix(resource, "/*") {
					rule.PrefixRules = append(rule.PrefixRules, strings.TrimSuffix(resource, "*"))
					continue
				}
				rule.ExactRules[resource] = struct{}{}
				continue
			}
			rule.ServiceNames[resource] = struct{}{}
		}

		rules[clientID] = rule
	}
	return rules
}

func splitResources(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, item := range parts {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func cloneRules(src map[string]model.ClientRule) map[string]model.ClientRule {
	dst := make(map[string]model.ClientRule, len(src))
	for key, value := range src {
		cloned := value
		cloned.ServiceNames = make(map[string]struct{}, len(value.ServiceNames))
		for name := range value.ServiceNames {
			cloned.ServiceNames[name] = struct{}{}
		}
		cloned.ExactRules = make(map[string]struct{}, len(value.ExactRules))
		for name := range value.ExactRules {
			cloned.ExactRules[name] = struct{}{}
		}
		cloned.PrefixRules = append([]string(nil), value.PrefixRules...)
		dst[key] = cloned
	}
	return dst
}

func withJitter(base, jitterMax time.Duration) time.Duration {
	if base <= 0 || jitterMax <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(jitterMax)))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
