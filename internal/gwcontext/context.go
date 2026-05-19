package gwcontext

import (
	"context"

	"aevons-gateway/internal/model"

	"github.com/gin-gonic/gin"
)

type contextKey string

const requestContextKey contextKey = "gateway.request_context"

func WithRequestContext(ctx context.Context, rc *model.RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey, rc)
}

func FromContext(ctx context.Context) (*model.RequestContext, bool) {
	if ctx == nil {
		return nil, false
	}
	rc, ok := ctx.Value(requestContextKey).(*model.RequestContext)
	return rc, ok && rc != nil
}

func FromGin(c *gin.Context) (*model.RequestContext, bool) {
	if c == nil {
		return nil, false
	}
	rc, ok := c.Get(string(requestContextKey))
	if !ok {
		return nil, false
	}
	ctx, ok := rc.(*model.RequestContext)
	return ctx, ok && ctx != nil
}

func SetGin(c *gin.Context, rc *model.RequestContext) {
	if c == nil || rc == nil {
		return
	}
	c.Set(string(requestContextKey), rc)
	c.Request = c.Request.WithContext(WithRequestContext(c.Request.Context(), rc))
}
