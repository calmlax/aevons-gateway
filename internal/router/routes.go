package router

import (
	"fmt"
	"net/http"
	"time"

	gatewayswagger "aevons-gateway/internal/swagger"

	"aevons-gateway/internal/ratelimit"

	gatewayproxy "aevons-gateway/internal/proxy"

	"aevons-gateway/internal/discovery"

	gatewayconfig "aevons-gateway/internal/config"

	gatewayclientauth "aevons-gateway/internal/clientauth"

	gatewayauth "aevons-gateway/internal/auth"

	"github.com/calmlax/aevons-framework/core"
	frameworkconsul "github.com/calmlax/aevons-framework/core/consul"
	"github.com/calmlax/aevons-framework/core/server"
	"github.com/calmlax/aevons-framework/middleware"
	"github.com/gin-gonic/gin"
)

func Setup(app *core.App, settings gatewayconfig.Settings) (*gin.Engine, error) {
	cfg, err := app.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("router: read app config failed: %w", err)
	}
	db, err := app.RawDatabase()
	if err != nil {
		return nil, fmt.Errorf("router: read database failed: %w", err)
	}

	gin.SetMode(cfg.Server.Mode)

	matcher, err := NewServiceMatcher(settings.Services)
	if err != nil {
		return nil, fmt.Errorf("router: build service matcher failed: %w", err)
	}

	var registry *frameworkconsul.Registry
	if cfg.Consul.Enabled {
		registry, err = frameworkconsul.New(cfg.Consul)
		if err != nil {
			return nil, fmt.Errorf("router: init consul registry failed: %w", err)
		}
	}

	resolver := discovery.NewResolver(registry, settings.Gateway.Discovery)
	checker := gatewayclientauth.NewChecker(settings.ClientAuth.Enabled, db, 30*time.Second)
	verifier := gatewayauth.NewVerifier(resolver, "auth-service", 5*time.Second)
	limiter, err := ratelimit.New(settings.Gateway.RateLimit)
	if err != nil {
		return nil, fmt.Errorf("router: init rate limiter failed: %w", err)
	}
	proxyHandler := gatewayproxy.NewHandler(
		matcher,
		checker,
		resolver,
		verifier,
		limiter,
		time.Duration(settings.Gateway.TimeoutSeconds)*time.Second,
	)
	swaggerHandler := gatewayswagger.NewHandler(settings, resolver, matcher.Rules())

	r := gin.New()
	if err := r.SetTrustedProxies(settings.Gateway.TrustedProxies); err != nil {
		return nil, fmt.Errorf("router: set trusted proxies failed: %w", err)
	}

	r.Use(limitBodySize(settings.Gateway.MaxBodyBytes))
	r.Use(middleware.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.CORS(cfg.CORS.Enabled, cfg.CORS.AllowedOrigins))
	r.Use(middleware.XSSMiddleware(cfg))

	server.RegisterHealthRoute(r, cfg.Server.Name)
	server.RegisterOpenApiRoute(r, cfg)
	registerSwaggerUI(r, settings)

	v1 := r.Group("/api/v1/gateway")
	{
		v1.GET("/swagger/sources", swaggerHandler.Sources)
		v1.GET("/swagger/swagger-config", swaggerHandler.SwaggerConfig)
		v1.GET("/swagger/:service/swagger.json", swaggerHandler.Proxy)
	}

	r.NoRoute(proxyHandler.Forward)
	return r, nil
}

func registerSwaggerUI(r *gin.Engine, settings gatewayconfig.Settings) {
	if !settings.Swagger.Enabled || !settings.Swagger.UIEnabled {
		return
	}
	r.GET("/swagger", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/swagger/")
	})
	swaggerGroup := r.Group("/swagger", swaggerAccess(settings))
	swaggerGroup.Static("/", "./swagger-ui")
}

func limitBodySize(maxBodyBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBodyBytes > 0 {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		}
		c.Next()
	}
}

func swaggerAccess(settings gatewayconfig.Settings) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(settings.Swagger.AllowedIPs) == 0 {
			c.Next()
			return
		}
		clientIP := c.ClientIP()
		for _, allowed := range settings.Swagger.AllowedIPs {
			if allowed == "*" || allowed == clientIP {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code":      "GATEWAY_SWAGGER_FORBIDDEN",
			"message":   "gateway.swagger_forbidden",
			"requestId": c.GetString(middleware.RequestIdKey),
			"data":      nil,
		})
	}
}
