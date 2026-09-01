package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func registerCaoliaoRoutes(apiRouter *gin.RouterGroup) {
	caoliaoRoute := apiRouter.Group("/caoliao/v1")
	caoliaoRoute.Use(middleware.CaoliaoIntegrationAuth(), middleware.AnonymousRequestBodyLimit())
	{
		caoliaoRoute.GET("/health", controller.GetCaoliaoHealth)
		caoliaoRoute.PUT("/employees/:employee_id", controller.PutCaoliaoEmployee)
		caoliaoRoute.GET("/employees/:employee_id/keys", controller.GetCaoliaoEmployeeKeys)
		caoliaoRoute.POST("/employees/:employee_id/keys", controller.PostCaoliaoEmployeeKey)
		caoliaoRoute.PATCH("/keys/:id", controller.PatchCaoliaoKey)
		caoliaoRoute.DELETE("/keys/:id", controller.DeleteCaoliaoKey)
		caoliaoRoute.GET("/usage", controller.GetCaoliaoUsage)
		caoliaoRoute.POST("/usage/mock", controller.PostCaoliaoMockUsage)
	}
}
