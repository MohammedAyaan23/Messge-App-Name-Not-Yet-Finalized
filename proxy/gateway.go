package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func Gateway() {
	fmt.Println("Hello World")
}

func NewBucket(refill float64, capacity int) *Bucket {
	return &Bucket{
		refillRate:      refill,
		capacity:        capacity,
		tokensAvailable: capacity,
		lastRefill:      time.Now(),
	}
}

type Bucket struct {
	refillRate              float64
	capacity                int
	tokensAvailable         int
	lastRefill              time.Time
	mu                      sync.Mutex
	totatlRequestsProcessed int
	totalRequestsDropped    int
}

var bucket *Bucket = NewBucket(2, 20)

func (b *Bucket) AllowRequest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsedSeconds := now.Sub(b.lastRefill).Seconds()
	refilledTokens := int(elapsedSeconds * b.refillRate)

	// Only update state if we actually refilled
	if refilledTokens > 0 {
		b.tokensAvailable += refilledTokens
		if b.tokensAvailable > b.capacity {
			b.tokensAvailable = b.capacity
		}
		b.lastRefill = now
	}

	// Check and consume token
	if b.tokensAvailable >= 1 {
		b.tokensAvailable--
		b.totatlRequestsProcessed++
		return true
	}

	b.totalRequestsDropped++
	return false
}

func rateLimitMiddleware(next http.Handler, bucket *Bucket) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bucket.AllowRequest() {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	backendURL, err := url.Parse("http://main-app:8080")
	if err != nil {
		log.Fatalf("Failed to parse backend URL: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(backendURL)

	// Wrap the proxy with rate limiting
	rateLimitedProxy := rateLimitMiddleware(proxy, bucket)

	// Create a custom mux
	mux := http.NewServeMux()
	mux.Handle("/signup", rateLimitedProxy)
	mux.Handle("/login", rateLimitedProxy)
	mux.Handle("/refresh", rateLimitedProxy)
	mux.Handle("/api/search", rateLimitedProxy)
	mux.Handle("/api/conversation", rateLimitedProxy)
	mux.Handle("/ws", rateLimitedProxy)

	// Add a health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Create HTTP server with configuration
	server := &http.Server{
		Addr:    ":8030",
		Handler: mux,
		// Add timeouts for better security and graceful shutdown
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Channel to listen for server errors
	serverErrors := make(chan error, 1)

	// Start server in a goroutine
	go func() {
		log.Printf("API Gateway starting on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrors <- err
		}
	}()

	// Channel to listen for OS signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// Wait for either a server error or shutdown signal
	select {
	case err := <-serverErrors:
		log.Fatalf("Server error: %v", err)

	case sig := <-shutdown:
		log.Printf("Received %v signal. Starting graceful shutdown...", sig)
		log.Printf("Total requests arrived in the gateway: %d", bucket.totatlRequestsProcessed+bucket.totalRequestsDropped)
		log.Printf("Total requests processed: %d", bucket.totatlRequestsProcessed)
		log.Printf("Total requests dropped: %d", bucket.totalRequestsDropped)
		// Create a context with timeout for shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Attempt graceful shutdown
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Graceful shutdown failed: %v", err)

			// Force close if graceful shutdown fails after timeout
			log.Println("Forcing server closure...")
			if err := server.Close(); err != nil {
				log.Printf("Server forced closure error: %v", err)
			}
		}

		log.Println("Server shutdown completed gracefully")
	}
}
