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
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris protection
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      120 * time.Second, // Image proxy can stream large files to slow clients
		IdleTimeout:       120 * time.Second,
	}

	if err := srvObj.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
