package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/calmlax/aevons-gateway/internal/discovery"
	"github.com/calmlax/aevons-gateway/internal/model"

	frameworkresp "github.com/calmlax/aevons-framework/response"
	"github.com/gin-gonic/gin"
)

type Verifier struct {
	resolver        *discovery.Resolver
	authServiceName string
	authRule        model.ServiceRule
	httpClient      *http.Client
}

func NewVerifier(resolver *discovery.Resolver, authServiceName string, timeout time.Duration) *Verifier {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Verifier{
		resolver:        resolver,
		authServiceName: authServiceName,
		authRule: model.ServiceRule{
			Name:        authServiceName,
			Discovery:   "consul",
			LoadBalance: "round_robin",
		},
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (v *Verifier) Verify(ctx context.Context, rule *model.ServiceRule, authHeader string) (*model.UserContext, error) {
	if strings.TrimSpace(authHeader) == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, errors.New("authorization.token.missing")
	}

	instance, err := v.resolver.Resolve(&v.authRule)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+instance.Address+":"+strconv.Itoa(instance.Port)+"/api/auth/v1/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authHeader)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("authorization.token.invalid")
	}

	var payload struct {
		Code int               `json:"code"`
		Data model.UserContext `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Code != 0 {
		return nil, errors.New("authorization.token.invalid")
	}

	return &payload.Data, nil
}

func ReplyUnauthorized(c *gin.Context, message string) {
	frameworkresp.Fail(c, http.StatusUnauthorized, http.StatusUnauthorized, message)
}

func ReplyForbidden(c *gin.Context, message string) {
	frameworkresp.Fail(c, http.StatusForbidden, http.StatusForbidden, message)
}
