package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"binternet-go/internal/config"
	"binternet-go/internal/server"
)

func main() {
	cfg := config.Load()

	srv, err := server.NewServer(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	log.Printf("Starting Binternet server on %s", addr)

	srvObj := &http.Server{
		Addr:         addr,
		Handler:      srv,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if err := srvObj.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
