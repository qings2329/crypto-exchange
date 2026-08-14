package response

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// JSON 返回统一结构的成功响应。
func JSON(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "ok",
		"data":    data,
	})
}

// Error 返回统一结构的错误响应。
func Error(c *gin.Context, httpStatus, code int, msg string) {
	c.JSON(httpStatus, gin.H{
		"code":    code,
		"message": msg,
		"data":    nil,
	})
}
