package model

type GatewayConfig struct {
	TrustedProxies []string        `yaml:"trusted_proxies"`
	TimeoutSeconds int             `yaml:"timeout_seconds"`
	MaxBodyBytes   int64           `yaml:"max_body_bytes"`
	Discovery      DiscoveryConfig `yaml:"discovery"`
	RateLimit      RateLimitConfig `yaml:"rate_limit"`
}

type DiscoveryConfig struct {
	RefreshSeconds      int `yaml:"refresh_seconds"`
	StaleIfErrorSeconds int `yaml:"stale_if_error_seconds"`
}

type RateLimitConfig struct {
	Enabled     bool                  `yaml:"enabled"`
	FailOpen    bool                  `yaml:"fail_open"`
	KeyPrefix   string                `yaml:"key_prefix"`
	ConsulKVKey string                `yaml:"consul_kv_key"`
	Default     RateLimitRuleConfig   `yaml:"default"`
	Rules       []RateLimitRuleConfig `yaml:"rules"`
}

type RateLimitRuleConfig struct {
	Name          string   `yaml:"name"`
	Enabled       bool     `yaml:"enabled"`
	Services      []string `yaml:"services"`
	Methods       []string `yaml:"methods"`
	Paths         []string `yaml:"paths"`
	KeyBy         []string `yaml:"key_by"`
	Limit         int64    `yaml:"limit"`
	WindowSeconds int64    `yaml:"window_seconds"`
}

type SwaggerConfig struct {
	Enabled    bool               `yaml:"enabled"`
	UIEnabled  bool               `yaml:"ui_enabled"`
	AllowedIPs []string           `yaml:"allowed_ips"`
	Docs       []SwaggerDocConfig `yaml:"docs"`
}

type SwaggerDocConfig struct {
	Name      string `yaml:"name"`
	ServiceID string `yaml:"service_id"`
	Path      string `yaml:"path"`
}

type ServiceConfig struct {
	ID                string   `yaml:"id"`
	Name              string   `yaml:"name"`
	Prefix            string   `yaml:"prefix"`
	Discovery         string   `yaml:"discovery"`
	LoadBalance       string   `yaml:"load_balance"`
	PassAccessToken   bool     `yaml:"pass_access_token"`
	ExcludeAuthRoutes []string `yaml:"exclude_auth_routes"`
}

type ClientRuleConfig struct {
	ClientID  string   `yaml:"client_id"`
	Enabled   bool     `yaml:"enabled"`
	Resources []string `yaml:"resources"`
}

type ClientAuthConfig struct {
	Enabled bool `yaml:"enabled"`
}

type RouteRule struct {
	Method    string
	Pattern   string
	IsPrefix  bool
	RawSource string
}

type RateLimitRule struct {
	Name          string
	Enabled       bool
	ServiceNames  map[string]struct{}
	Methods       map[string]struct{}
	PathRules     []RouteRule
	KeyBy         []string
	Limit         int64
	WindowSeconds int64
}

type ServiceRule struct {
	ID               string
	Name             string
	Prefix           string
	MatchPrefix      string
	Discovery        string
	LoadBalance      string
	PassAccessToken  bool
	ExcludeAuthRules []RouteRule
}

type ClientRule struct {
	ClientID     string
	Enabled      bool
	AllowAll     bool
	ServiceNames map[string]struct{}
	ExactRules   map[string]struct{}
	PrefixRules  []string
}

type ClientIdentity struct {
	ClientID string `json:"client_id"`
	Source   string `json:"source"`
}

type SwaggerSource struct {
	Name      string `json:"name"`
	Service   string `json:"service"`
	TargetURL string `json:"target_url"`
	ProxyURL  string `json:"proxy_url"`
}

type UserIdentity struct {
	UserID      string   `json:"user_id"`
	Username    string   `json:"username"`
	Nickname    string   `json:"nickname"`
	ClientID    string   `json:"client_id"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
}

type UserContext struct {
	UserID      string   `json:"user_id"`
	Username    string   `json:"username"`
	Nickname    string   `json:"nickname"`
	ClientID    string   `json:"client_id"`
	Permissions []string `json:"permissions"`
	Roles       any      `json:"roles"`
	Depts       any      `json:"depts"`
}

type RequestContext struct {
	RequestID string
	Service   *ServiceRule
	Client    *ClientIdentity
	User      *UserIdentity
}

type RateLimitDecision struct {
	Allowed    bool
	RuleName   string
	Limit      int64
	Remaining  int64
	ResetAfter int64
	StatusCode int
	Message    string
}
