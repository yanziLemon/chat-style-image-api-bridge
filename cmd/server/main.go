package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"github.com/your-org/chat-style-image-api-bridge/internal/bridge"
)

func main() {
	cfg := bridge.LoadConfig()
	if cfg.UpstreamBaseURL == "" {
		log.Fatal("UPSTREAM_BASE_URL is required")
	}
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())
	bridge.New(cfg).Register(router)
	log.Printf("chat-style-image-api-bridge listening on %s", cfg.ListenAddr)
	if err := router.Run(cfg.ListenAddr); err != nil {
		log.Fatal(err)
	}
}
