package response

import "github.com/gin-gonic/gin"

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
	Data      any    `json:"data"`
}

func Fail(c *gin.Context, httpStatus int, code, message string) {
	requestID := c.GetHeader("X-Request-ID")
	if requestID == "" {
		requestID = c.Writer.Header().Get("X-Request-ID")
	}
	c.JSON(httpStatus, ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: requestID,
		Data:      nil,
	})
}
