package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// Version is set at build time via -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	// Command line flags
	pttPort := flag.Int("ptt-port", 5000, "UDP port for PTT communication")
	webPort := flag.Int("web-port", 8080, "HTTP port for web GUI")
	configFile := flag.String("config", "config.json", "Configuration file path")
	flag.Parse()

	// Load configuration
	config, err := LoadConfig(*configFile)
	if err != nil {
		log.Printf("Warning: Could not load config file: %v. Using defaults.", err)
		config = DefaultConfig()
	}

	// Override with command line flags
	if *pttPort != 5000 {
		config.PTTPort = *pttPort
	}
	if *webPort != 8080 {
		config.WebPort = *webPort
	}

	// Version already carries its "v" (build.sh uses `git describe --tags`),
	// so don't prefix another one — that printed "vv3.3.0".
	log.Printf("=== MeshRelay Server %s Starting ===", Version)
	log.Printf("PTT Port: %d", config.PTTPort)
	log.Printf("Web GUI: http://localhost:%d", config.WebPort)
	log.Printf("Max Clients: %d", config.MaxClients)
	log.Printf("Max Channels: %d", config.MaxChannels)

	// Create server instance
	server := NewServer(config, *configFile)

	// Start PTT UDP server
	go func() {
		if err := server.StartPTT(); err != nil {
			log.Fatalf("Failed to start PTT server: %v", err)
		}
	}()

	// Start web GUI server
	go func() {
		if err := server.StartWeb(); err != nil {
			log.Fatalf("Failed to start web server: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down server...")
	server.Shutdown()
	log.Println("Server stopped")
}
