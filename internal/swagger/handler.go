package swagger

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gatewayresp "aevons-gateway/internal/response"

	"aevons-gateway/internal/model"

	"aevons-gateway/internal/discovery"

	gatewayconfig "aevons-gateway/internal/config"

	frameworkresp "github.com/calmlax/aevons-framework/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	settings   gatewayconfig.Settings
	resolver   *discovery.Resolver
	services   map[string]*model.ServiceRule
	httpClient *http.Client
}

func NewHandler(settings gatewayconfig.Settings, resolver *discovery.Resolver, services []*model.ServiceRule) *Handler {
	serviceMap := make(map[string]*model.ServiceRule, len(services))
	for _, service := range services {
		if service == nil {
			continue
		}
		if service.ID != "" {
			serviceMap[service.ID] = service
		}
		if service.Name != "" {
			serviceMap[service.Name] = service
		}
	}
	return &Handler{
		settings: settings,
		resolver: resolver,
		services: serviceMap,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (h *Handler) Sources(c *gin.Context) {
	if !h.settings.Swagger.Enabled {
		gatewayresp.Fail(c, http.StatusNotFound, "GATEWAY_SWAGGER_DISABLED", "gateway.swagger_disabled")
		return
	}
	if !h.allow(c.ClientIP()) {
		gatewayresp.Fail(c, http.StatusForbidden, "GATEWAY_SWAGGER_FORBIDDEN", "gateway.swagger_forbidden")
		return
	}

	sources := make([]model.SwaggerSource, 0, len(h.settings.Swagger.Docs))
	for _, doc := range h.settings.Swagger.Docs {
		if doc.ServiceID == "" {
			continue
		}
		sources = append(sources, model.SwaggerSource{
			Name:      doc.Name,
			Service:   doc.ServiceID,
			TargetURL: doc.ServiceID + doc.Path,
			ProxyURL:  "/api/v1/gateway/swagger/" + doc.ServiceID + "/swagger.json",
		})
	}

	frameworkresp.Success(c, gin.H{
		"enabled": h.settings.Swagger.Enabled,
		"sources": sources,
	})
}

func (h *Handler) Proxy(c *gin.Context) {
	if !h.settings.Swagger.Enabled {
		gatewayresp.Fail(c, http.StatusNotFound, "GATEWAY_SWAGGER_DISABLED", "gateway.swagger_disabled")
		return
	}
	if !h.allow(c.ClientIP()) {
		gatewayresp.Fail(c, http.StatusForbidden, "GATEWAY_SWAGGER_FORBIDDEN", "gateway.swagger_forbidden")
		return
	}

	serviceID := c.Param("service")
	source, ok := h.lookup(serviceID)
	if !ok {
		gatewayresp.Fail(c, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "gateway.swagger_source_not_found")
		return
	}

	targetURL, err := h.resolveTargetURL(source)
	if err != nil {
		gatewayresp.Fail(c, http.StatusBadGateway, "GATEWAY_PROXY_ERROR", "gateway.swagger_proxy_failed")
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		gatewayresp.Fail(c, http.StatusInternalServerError, "GATEWAY_PROXY_ERROR", "gateway.swagger_proxy_failed")
		return
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		gatewayresp.Fail(c, http.StatusBadGateway, "GATEWAY_PROXY_ERROR", "gateway.swagger_proxy_failed")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		gatewayresp.Fail(c, http.StatusInternalServerError, "GATEWAY_PROXY_ERROR", "gateway.swagger_proxy_failed")
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	c.Data(resp.StatusCode, contentType, body)
}

func (h *Handler) SwaggerConfig(c *gin.Context) {
	if !h.settings.Swagger.Enabled || !h.settings.Swagger.UIEnabled {
		gatewayresp.Fail(c, http.StatusNotFound, "GATEWAY_SWAGGER_DISABLED", "gateway.swagger_disabled")
		return
	}
	if !h.allow(c.ClientIP()) {
		gatewayresp.Fail(c, http.StatusForbidden, "GATEWAY_SWAGGER_FORBIDDEN", "gateway.swagger_forbidden")
		return
	}

	urls := make([]gin.H, 0, len(h.settings.Swagger.Docs))
	for _, doc := range h.settings.Swagger.Docs {
		urls = append(urls, gin.H{
			"name": doc.Name,
			"url":  "/api/v1/gateway/swagger/" + doc.ServiceID + "/swagger.json",
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"urls":             urls,
		"urls.primaryName": firstDocName(h.settings.Swagger.Docs),
	})
}

func (h *Handler) lookup(serviceID string) (model.SwaggerDocConfig, bool) {
	for _, doc := range h.settings.Swagger.Docs {
		if doc.ServiceID == serviceID {
			return doc, true
		}
	}
	return model.SwaggerDocConfig{}, false
}

func (h *Handler) resolveTargetURL(source model.SwaggerDocConfig) (string, error) {
	serviceRule, ok := h.services[source.ServiceID]
	if !ok {
		return "", errors.New("swagger service rule not found")
	}
	instance, err := h.resolver.Resolve(serviceRule)
	if err != nil {
		return "", err
	}
	path := source.Path
	if strings.TrimSpace(path) == "" {
		path = "/api/swagger.json"
	}
	u := url.URL{
		Scheme: "http",
		Host:   instance.Address + ":" + strconv.Itoa(instance.Port),
		Path:   path,
	}
	return u.String(), nil
}

func (h *Handler) allow(clientIP string) bool {
	if len(h.settings.Swagger.AllowedIPs) == 0 {
		return true
	}
	for _, allowed := range h.settings.Swagger.AllowedIPs {
		allowed = strings.TrimSpace(allowed)
		if allowed == "*" || allowed == clientIP {
			return true
		}
	}
	return false
}

func firstDocName(docs []model.SwaggerDocConfig) string {
	if len(docs) == 0 {
		return ""
	}
	return docs[0].Name
}
