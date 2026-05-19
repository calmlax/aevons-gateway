package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/calmlax/aevons-gateway/internal/model"

	frameworkredis "github.com/calmlax/aevons-framework/redis"
	"github.com/calmlax/aevons-framework/xlog"
)

// fixedWindowScript implements a fixed-window counter in Redis.
// Each time window uses a different key suffix, so rollover is handled by key rotation.
const fixedWindowScript = `
local current = redis.call("INCR", KEYS[1])
if current == 1 then
	redis.call("EXPIRE", KEYS[1], ARGV[1])
end
local ttl = redis.call("TTL", KEYS[1])
return {current, ttl}
`

const (
	DefaultStatusCode = 429
	DefaultMessage    = "gateway.rate_limit_exceeded"
)

type Limiter struct {
	enabled  bool
	failOpen bool
	prefix   string
	def      *model.RateLimitRule
	rules    []model.RateLimitRule
}

// Input is the minimum request context needed to match a rule and build a bucket key.
type Input struct {
	Method  string
	Path    string
	IP      string
	Service *model.ServiceRule
	Client  *model.ClientIdentity
	User    *model.UserIdentity
}

// New compiles static config into runtime-ready rules so request-time checks stay lightweight.
func New(cfg model.RateLimitConfig) (*Limiter, error) {
	limiter := &Limiter{
		enabled:  cfg.Enabled,
		failOpen: cfg.FailOpen,
		prefix:   strings.TrimSpace(cfg.KeyPrefix),
	}
	if limiter.prefix == "" {
		limiter.prefix = "gateway:rate-limit:"
	}
	if !cfg.Enabled {
		return limiter, nil
	}

	if cfg.Default.Enabled {
		rule, err := compileRule(cfg.Default, true)
		if err != nil {
			return nil, err
		}
		limiter.def = rule
	}

	compiled := make([]model.RateLimitRule, 0, len(cfg.Rules))
	for _, raw := range cfg.Rules {
		rule, err := compileRule(raw, false)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, *rule)
	}
	limiter.rules = compiled
	return limiter, nil
}

// Allow finds the effective rule, increments the Redis counter for the current time bucket,
// and returns the decision consumed by the proxy layer.
func (l *Limiter) Allow(ctx context.Context, input Input) (model.RateLimitDecision, error) {
	if l == nil || !l.enabled {
		return model.RateLimitDecision{Allowed: true}, nil
	}

	rule := l.matchRule(input)
	if rule == nil || !rule.Enabled {
		return model.RateLimitDecision{Allowed: true}, nil
	}

	identity := buildIdentity(rule, input)
	bucket := time.Now().Unix() / rule.WindowSeconds
	serviceName := "unknown"
	if input.Service != nil && input.Service.Name != "" {
		serviceName = input.Service.Name
	}
	key := fmt.Sprintf("%s%s:%s:%d", l.prefix, rule.Name, identityKey(serviceName, identity), bucket)

	result, err := frameworkredis.Eval(ctx, fixedWindowScript, []string{key}, rule.WindowSeconds)
	if err != nil {
		if l.failOpen {
			xlog.Warn("gateway rate limit failed open (rule=%s key=%s): %v", rule.Name, key, err)
			return model.RateLimitDecision{Allowed: true}, nil
		}
		return model.RateLimitDecision{}, err
	}

	current, ttl := parseEvalResult(result)
	remaining := rule.Limit - current
	if remaining < 0 {
		remaining = 0
	}

	decision := model.RateLimitDecision{
		Allowed:    current <= rule.Limit,
		RuleName:   rule.Name,
		Limit:      rule.Limit,
		Remaining:  remaining,
		ResetAfter: ttl,
		StatusCode: DefaultStatusCode,
		Message:    DefaultMessage,
	}
	return decision, nil
}

// matchRule returns the first enabled specific rule that matches the request.
// If nothing matches, the default rule is used.
func (l *Limiter) matchRule(input Input) *model.RateLimitRule {
	for i := range l.rules {
		rule := &l.rules[i]
		if !rule.Enabled {
			continue
		}
		if matches(rule, input) {
			return rule
		}
	}
	return l.def
}

// matches evaluates the static selectors of a rule: service, method and path.
// Identity dimensions such as client, user and ip are applied later when building the bucket key.
func matches(rule *model.RateLimitRule, input Input) bool {
	if rule == nil {
		return false
	}
	if len(rule.ServiceNames) > 0 {
		if input.Service == nil {
			return false
		}
		if _, ok := rule.ServiceNames[input.Service.Name]; !ok {
			return false
		}
	}
	if len(rule.Methods) > 0 {
		if _, ok := rule.Methods[strings.ToUpper(input.Method)]; !ok {
			return false
		}
	}
	if len(rule.PathRules) > 0 {
		matched := false
		for _, item := range rule.PathRules {
			if item.Method != "ANY" && item.Method != strings.ToUpper(input.Method) {
				continue
			}
			if item.IsPrefix && strings.HasPrefix(input.Path, item.Pattern) {
				matched = true
				break
			}
			if !item.IsPrefix && input.Path == item.Pattern {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// compileRule normalizes raw config into a runtime rule:
// methods are uppercased, paths are converted to exact/prefix matchers,
// and key_by falls back to [client, ip] when omitted.
func compileRule(raw model.RateLimitRuleConfig, isDefault bool) (*model.RateLimitRule, error) {
	name := strings.TrimSpace(raw.Name)
	if isDefault && name == "" {
		name = "default"
	}
	if name == "" {
		return nil, fmt.Errorf("invalid rate limit rule: missing name")
	}

	serviceNames := make(map[string]struct{}, len(raw.Services))
	for _, item := range raw.Services {
		item = strings.TrimSpace(item)
		if item != "" {
			serviceNames[item] = struct{}{}
		}
	}

	methods := make(map[string]struct{}, len(raw.Methods))
	for _, item := range raw.Methods {
		item = strings.ToUpper(strings.TrimSpace(item))
		if item != "" {
			methods[item] = struct{}{}
		}
	}

	pathRules := make([]model.RouteRule, 0, len(raw.Paths))
	for _, item := range raw.Paths {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pathRules = append(pathRules, model.RouteRule{
			Method:    "ANY",
			Pattern:   strings.TrimSuffix(item, "**"),
			IsPrefix:  strings.HasSuffix(item, "**"),
			RawSource: item,
		})
	}

	keyBy := make([]string, 0, len(raw.KeyBy))
	if len(raw.KeyBy) == 0 {
		keyBy = append(keyBy, "client", "ip")
	} else {
		for _, item := range raw.KeyBy {
			item = strings.ToLower(strings.TrimSpace(item))
			if item != "" {
				keyBy = append(keyBy, item)
			}
		}
	}

	return &model.RateLimitRule{
		Name:          name,
		Enabled:       raw.Enabled,
		ServiceNames:  serviceNames,
		Methods:       methods,
		PathRules:     pathRules,
		KeyBy:         keyBy,
		Limit:         raw.Limit,
		WindowSeconds: raw.WindowSeconds,
	}, nil
}

// buildIdentity translates key_by dimensions into a stable bucket identity.
// The resulting parts are appended to the Redis key together with service and time bucket.
func buildIdentity(rule *model.RateLimitRule, input Input) []string {
	parts := make([]string, 0, len(rule.KeyBy))
	for _, key := range rule.KeyBy {
		switch key {
		case "global":
			parts = append(parts, "global")
		case "service":
			if input.Service != nil && input.Service.Name != "" {
				parts = append(parts, "service="+input.Service.Name)
			} else {
				parts = append(parts, "service=unknown")
			}
		case "client":
			if input.Client != nil && input.Client.ClientID != "" {
				parts = append(parts, "client="+input.Client.ClientID)
			} else {
				parts = append(parts, "client=anonymous")
			}
		case "user":
			if input.User != nil && input.User.UserID != "" {
				parts = append(parts, "user="+input.User.UserID)
			} else {
				parts = append(parts, "user=anonymous")
			}
		case "ip":
			if input.IP != "" {
				parts = append(parts, "ip="+input.IP)
			} else {
				parts = append(parts, "ip=unknown")
			}
		case "path":
			parts = append(parts, "path="+input.Path)
		case "method":
			parts = append(parts, "method="+strings.ToUpper(input.Method))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "global")
	}
	return parts
}

func identityKey(service string, parts []string) string {
	return service + ":" + strings.Join(parts, ":")
}

func parseEvalResult(value any) (count int64, ttl int64) {
	list, ok := value.([]any)
	if !ok || len(list) < 2 {
		return 0, 0
	}
	count = parseInt64(list[0])
	ttl = parseInt64(list[1])
	if ttl < 0 {
		ttl = 0
	}
	return count, ttl
}

func parseInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case uint64:
		return int64(typed)
	default:
		return 0
	}
}
