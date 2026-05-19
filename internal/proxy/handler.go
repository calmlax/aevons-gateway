package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	gatewayauth "github.com/calmlax/aevons-gateway/internal/auth"

	gatewayresp "github.com/calmlax/aevons-gateway/internal/response"

	"github.com/calmlax/aevons-gateway/internal/ratelimit"

	"github.com/calmlax/aevons-gateway/internal/model"

	"github.com/calmlax/aevons-gateway/internal/gwcontext"

	"github.com/calmlax/aevons-gateway/internal/discovery"

	"github.com/calmlax/aevons-framework/middleware"
	"github.com/calmlax/aevons-framework/xlog"
	"github.com/calmlax/aevons-gateway/internal/clientauth"
	"github.com/gin-gonic/gin"
)

type matcher interface {
	Match(path string) (*model.ServiceRule, bool)
}

type Handler struct {
	matcher    matcher
	checker    *clientauth.Checker
	resolverID *clientauth.Resolver
	resolver   *discovery.Resolver
	verifier   *gatewayauth.Verifier
	limiter    *ratelimit.Limiter
	httpClient *http.Transport
}

// NewHandler wires the gateway proxy pipeline:
// route matching, client auth, token verification, rate limiting and upstream forwarding.
func NewHandler(
	matcher matcher,
	checker *clientauth.Checker,
	resolver *discovery.Resolver,
	verifier *gatewayauth.Verifier,
	limiter *ratelimit.Limiter,
	timeout time.Duration,
) *Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout

	return &Handler{
		matcher:    matcher,
		checker:    checker,
		resolverID: clientauth.NewResolver(),
		resolver:   resolver,
		verifier:   verifier,
		limiter:    limiter,
		httpClient: transport,
	}
}

// Forward is the main request pipeline of the gateway.
// The order is deliberate:
// route match -> public route check -> user auth -> client identity -> client authorization
// -> rate limit -> service discovery -> reverse proxy.
func (h *Handler) Forward(c *gin.Context) {
	startedAt := time.Now()
	rule, ok := h.matcher.Match(c.Request.URL.Path)
	if !ok {
		gatewayresp.Fail(c, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "gateway.route_not_found")
		return
	}

	authHeader := c.GetHeader("Authorization")
	publicRoute := isExcluded(rule, c.Request.Method, c.Request.URL.Path)
	// RequestContext is shared across downstream gateway components so logging,
	// proxying and governance logic can use the same resolved identities.
	requestCtx := &model.RequestContext{
		RequestID: c.GetString("X-Request-ID"),
		Service:   rule,
	}
	gwcontext.SetGin(c, requestCtx)

	var user *model.UserIdentity
	if !publicRoute {
		// Protected routes must pass token verification before any client/resource checks.
		verified, err := h.verifier.Verify(c.Request.Context(), rule, authHeader)
		if err != nil {
			gatewayresp.Fail(c, http.StatusUnauthorized, "GATEWAY_TOKEN_INVALID", err.Error())
			return
		}
		user = toUserIdentity(verified)
		requestCtx.User = user
		gwcontext.SetGin(c, requestCtx)
	}

	// Client identity is resolved even for public routes, because client-based governance
	// such as oauth_client resource checks and rate limiting still depends on it.
	client, err := h.resolverID.Resolve(c.Request.Header, user)
	if err != nil {
		gatewayresp.Fail(c, http.StatusUnauthorized, "GATEWAY_CLIENT_INVALID", err.Error())
		return
	}
	requestCtx.Client = client
	gwcontext.SetGin(c, requestCtx)

	clientID := ""
	if client != nil {
		clientID = client.ClientID
	}

	allowedByAll := false
	if clientID != "" {
		// Resource authorization primarily uses configured path rules, while still
		// keeping service-name rules as a backward-compatible fallback.
		allowed, allowAll := h.checkWithMode(clientID, rule, c.Request.URL.Path)
		if !allowed {
			gatewayresp.Fail(c, http.StatusForbidden, "GATEWAY_CLIENT_FORBIDDEN", "gateway.client_resource_forbidden")
			h.logAudit(c, startedAt, requestCtx, "", http.StatusForbidden, false, true)
			return
		}
		allowedByAll = allowAll
	}

	if h.limiter != nil {
		// Rate limiting runs after identities are resolved, so rules can bucket by
		// client/user/ip/service/path/method using the same normalized gateway context.
		decision, err := h.limiter.Allow(c.Request.Context(), ratelimit.Input{
			Method:  c.Request.Method,
			Path:    c.Request.URL.Path,
			IP:      c.ClientIP(),
			Service: rule,
			Client:  client,
			User:    user,
		})
		if err != nil {
			xlog.Error("gateway rate limit failed: %v", err)
			gatewayresp.Fail(c, http.StatusInternalServerError, "GATEWAY_RATE_LIMIT_ERROR", "gateway.rate_limit_error")
			h.logAudit(c, startedAt, requestCtx, "", http.StatusInternalServerError, allowedByAll, false)
			return
		}
		if decision.Limit > 0 {
			c.Header("X-RateLimit-Limit", strconv.FormatInt(decision.Limit, 10))
			c.Header("X-RateLimit-Remaining", strconv.FormatInt(decision.Remaining, 10))
			c.Header("X-RateLimit-Reset", strconv.FormatInt(decision.ResetAfter, 10))
		}
		if !decision.Allowed {
			if decision.ResetAfter > 0 {
				c.Header("Retry-After", strconv.FormatInt(decision.ResetAfter, 10))
			}
			gatewayresp.Fail(c, decision.StatusCode, "GATEWAY_RATE_LIMITED", decision.Message)
			h.logRateLimit(c, startedAt, requestCtx, decision)
			return
		}
	}

	// Upstream instances are resolved late so auth/authorization/limit failures do not
	// waste service-discovery work or create unnecessary upstream pressure.
	instance, err := h.resolver.Resolve(rule)
	if err != nil {
		xlog.Error("gateway resolve %s failed: %v", rule.Name, err)
		gatewayresp.Fail(c, http.StatusServiceUnavailable, "GATEWAY_SERVICE_UNAVAILABLE", "gateway.upstream_unavailable")
		h.logAudit(c, startedAt, requestCtx, "", http.StatusServiceUnavailable, allowedByAll, false)
		return
	}

	target, err := url.Parse("http://" + instance.Address)
	if err != nil {
		gatewayresp.Fail(c, http.StatusInternalServerError, "GATEWAY_PROXY_ERROR", "gateway.invalid_upstream")
		h.logAudit(c, startedAt, requestCtx, instance.Address+":"+strconv.Itoa(instance.Port), http.StatusInternalServerError, allowedByAll, false)
		return
	}

	proxy := &httputil.ReverseProxy{
		Transport: h.httpClient,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			xlog.Error("gateway proxy %s failed: %v", rule.Name, proxyErr)
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = rw.Write([]byte(`{"code":"GATEWAY_PROXY_ERROR","message":"gateway.proxy_failed","data":null}`))
		},
	}

	proxy.Rewrite = func(pr *httputil.ProxyRequest) {
		// The gateway rewrites the request into the selected upstream and injects the
		// normalized headers that downstream services rely on for context propagation.
		pr.SetURL(target)
		pr.Out.URL.Host = instance.Address + ":" + strconv.Itoa(instance.Port)
		pr.Out.Host = pr.Out.URL.Host

		pr.Out.Header.Set("X-Request-ID", c.GetString("X-Request-ID"))
		pr.Out.Header.Set("X-Client-IP", c.ClientIP())
		pr.Out.Header.Set("X-Forwarded-For", forwardedFor(c))
		pr.Out.Header.Set("X-Forwarded-Host", c.Request.Host)
		pr.Out.Header.Set("X-Forwarded-Proto", scheme(c))
		if client != nil {
			pr.Out.Header.Set("X-Client-Id", client.ClientID)
		}
		if user != nil {
			pr.Out.Header.Set("X-User-Id", user.UserID)
			pr.Out.Header.Set("X-Username", user.Username)
			pr.Out.Header.Set("X-Nickname", user.Nickname)
			pr.Out.Header.Set("X-Client-Id", user.ClientID)
			pr.Out.Header.Set("X-User-Permissions", strings.Join(user.Permissions, ","))
		}
		if !rule.PassAccessToken {
			pr.Out.Header.Del("Authorization")
		}
	}

	proxy.ServeHTTP(c.Writer, c.Request)
	h.logAudit(c, startedAt, requestCtx, instance.Address+":"+strconv.Itoa(instance.Port), c.Writer.Status(), allowedByAll, false)
}

func scheme(c *gin.Context) string {
	if c.Request.TLS != nil {
		return "https"
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	return "http"
}

func forwardedFor(c *gin.Context) string {
	if prior := strings.TrimSpace(c.GetHeader("X-Forwarded-For")); prior != "" {
		return prior + ", " + c.ClientIP()
	}
	return c.ClientIP()
}

// isExcluded checks whether a route is explicitly declared as public in service config.
func isExcluded(rule *model.ServiceRule, method, path string) bool {
	if rule == nil {
		return false
	}
	method = strings.ToUpper(method)
	for _, item := range rule.ExcludeAuthRules {
		if item.Method != method {
			continue
		}
		if item.IsPrefix && strings.HasPrefix(path, item.Pattern) {
			return true
		}
		if !item.IsPrefix && path == item.Pattern {
			return true
		}
	}
	return false
}

func toUserIdentity(user *model.UserContext) *model.UserIdentity {
	if user == nil {
		return nil
	}
	roles := make([]string, 0)
	switch typed := user.Roles.(type) {
	case []string:
		roles = append(roles, typed...)
	case []any:
		for _, item := range typed {
			if value, ok := item.(string); ok {
				roles = append(roles, value)
			}
		}
	}
	return &model.UserIdentity{
		UserID:      user.UserID,
		Username:    user.Username,
		Nickname:    user.Nickname,
		ClientID:    user.ClientID,
		Roles:       roles,
		Permissions: user.Permissions,
	}
}

func (h *Handler) checkWithMode(clientID string, service *model.ServiceRule, path string) (allowed bool, allowAll bool) {
	if h.checker == nil {
		return true, false
	}
	rule, ok := h.checker.Rule(clientID)
	if !ok {
		return h.checker.Allow(clientID, service, path), false
	}
	if rule.AllowAll {
		return true, true
	}
	return h.checker.Allow(clientID, service, path), false
}

func (h *Handler) logAudit(
	c *gin.Context,
	startedAt time.Time,
	requestCtx *model.RequestContext,
	instance string,
	status int,
	allowAll bool,
	denied bool,
) {
	serviceName := ""
	clientID := ""
	userID := ""
	if requestCtx != nil {
		if requestCtx.Service != nil {
			serviceName = requestCtx.Service.Name
		}
		if requestCtx.Client != nil {
			clientID = requestCtx.Client.ClientID
		}
		if requestCtx.User != nil {
			userID = requestCtx.User.UserID
		}
	}
	xlog.Info(
		"[gateway reqId:%s] [method:%s] [path:%s] [service:%s] [client:%s] [user:%s] [instance:%s] [status:%d] [allowAll:%t] [denied:%t] [latency:%.2fms]",
		c.GetString("X-Request-ID"),
		c.Request.Method,
		c.Request.URL.Path,
		serviceName,
		clientID,
		userID,
		instance,
		status,
		allowAll,
		denied,
		float64(time.Since(startedAt).Microseconds())/1000.0,
	)
}

func (h *Handler) logRateLimit(
	c *gin.Context,
	startedAt time.Time,
	requestCtx *model.RequestContext,
	decision model.RateLimitDecision,
) {
	serviceName := ""
	clientID := ""
	userID := ""
	if requestCtx != nil {
		if requestCtx.Service != nil {
			serviceName = requestCtx.Service.Name
		}
		if requestCtx.Client != nil {
			clientID = requestCtx.Client.ClientID
		}
		if requestCtx.User != nil {
			userID = requestCtx.User.UserID
		}
	}
	xlog.Warn(
		"[gateway rate-limit reqId:%s] [method:%s] [path:%s] [service:%s] [client:%s] [user:%s] [rule:%s] [limit:%d] [remaining:%d] [reset:%ds] [status:%d] [latency:%.2fms]",
		c.GetString(middleware.RequestIdKey),
		c.Request.Method,
		c.Request.URL.Path,
		serviceName,
		clientID,
		userID,
		decision.RuleName,
		decision.Limit,
		decision.Remaining,
		decision.ResetAfter,
		decision.StatusCode,
		float64(time.Since(startedAt).Microseconds())/1000.0,
	)
}
