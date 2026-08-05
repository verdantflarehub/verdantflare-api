package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetVideoRouter(router *gin.Engine) {
	// Video proxy: accepts either session auth (dashboard) or token auth (API clients)
	videoProxyRouter := router.Group("/v1")
	videoProxyRouter.Use(middleware.RouteTag("relay"))
	videoProxyRouter.Use(middleware.TokenOrUserAuthReadOnly())
	{
		videoProxyRouter.GET("/videos/:task_id/content", controller.VideoProxy)
	}

	videoSubmissionRouter := router.Group("/v1")
	videoSubmissionRouter.Use(middleware.RouteTag("relay"))
	videoSubmissionRouter.Use(middleware.TokenAuthReadOnly())
	{
		videoSubmissionRouter.GET("/video-submissions/:client_request_id", controller.GetVideoSubmission)
	}

	videoCreateRouter := router.Group("/v1")
	videoCreateRouter.Use(middleware.RouteTag("relay"))
	videoCreateRouter.Use(middleware.TokenAuth(), middleware.SD2SubmissionIdempotency(), middleware.Distribute())
	{
		videoCreateRouter.POST("/video/generations", controller.RelayTask)
		videoCreateRouter.POST("/videos/:video_id/remix", controller.RelayTask)
		videoCreateRouter.POST("/videos", controller.RelayTask)
	}

	videoReadRouter := router.Group("/v1")
	videoReadRouter.Use(middleware.RouteTag("relay"))
	videoReadRouter.Use(middleware.TokenAuthReadOnly(), middleware.Distribute())
	{
		videoReadRouter.GET("/video/generations/:task_id", controller.RelayTaskFetch)
		videoReadRouter.GET("/videos/:task_id", controller.RelayTaskFetch)
	}

	klingV1Router := router.Group("/kling/v1")
	klingV1Router.Use(middleware.RouteTag("relay"))
	klingV1Router.Use(middleware.KlingRequestConvert(), middleware.TokenAuth(), middleware.Distribute())
	{
		klingV1Router.POST("/videos/text2video", controller.RelayTask)
		klingV1Router.POST("/videos/image2video", controller.RelayTask)
		klingV1Router.GET("/videos/text2video/:task_id", controller.RelayTaskFetch)
		klingV1Router.GET("/videos/image2video/:task_id", controller.RelayTaskFetch)
	}

	// Jimeng official API routes - direct mapping to official API format
	jimengOfficialGroup := router.Group("jimeng")
	jimengOfficialGroup.Use(middleware.RouteTag("relay"))
	jimengOfficialGroup.Use(middleware.JimengRequestConvert(), middleware.TokenAuth(), middleware.Distribute())
	{
		// Maps to: /?Action=CVSync2AsyncSubmitTask&Version=2022-08-31 and /?Action=CVSync2AsyncGetResult&Version=2022-08-31
		jimengOfficialGroup.POST("/", controller.RelayTask)
	}
}
