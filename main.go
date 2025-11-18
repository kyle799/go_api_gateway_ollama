package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Job struct {
	w    http.ResponseWriter
	r    *http.Request
	id   string
	done chan struct{}
}

func main() {
	backendsEnv := os.Getenv("GATEWAY_BACKENDS")
	if backendsEnv == "" {
		log.Fatalf("GATEWAY_BACKENDS not set (example: http://127.0.0.1:11434,http://127.0.0.1:11435,http://127.0.0.1:11436)")
	}
	backendURLs := strings.Split(backendsEnv, ",")
	for i := range backendURLs {
		backendURLs[i] = strings.TrimSpace(backendURLs[i])
		if backendURLs[i] == "" {
			log.Fatalf("empty backend URL in GATEWAY_BACKENDS")
		}
		if _, err := url.Parse(backendURLs[i]); err != nil {
			log.Fatalf("invalid backend URL %q: %v", backendURLs[i], err)
		}
	}

	queueSize := 100
	if qs := os.Getenv("GATEWAY_QUEUE_SIZE"); qs != "" {
		// optional: parse int here if you want dynamic queue size
	}

	log.Printf("Starting gateway with %d backends, queue size %d", len(backendURLs), queueSize)

	jobCh := make(chan *Job, queueSize)

	// One worker per backend → max 1 concurrent job per backend
	for i, be := range backendURLs {
		go worker(i, be, jobCh)
	}

	mux := http.NewServeMux()

	// Main Ollama-compatible API entry point
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		jobID := time.Now().Format("20060102-150405.000000") + "-" + randomSuffix(6)
		log.Printf("[job %s] received %s %s from %s", jobID, r.Method, r.URL.Path, r.RemoteAddr)

		if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodOptions {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		job := &Job{
			w:    w,
			r:    r,
			id:   jobID,
			done: make(chan struct{}),
		}

		// Enqueue or fail if queue is full
		select {
		case jobCh <- job:
			// Wait until worker is done before handler returns
			<-job.done
			log.Printf("[job %s] finished", jobID)
		case <-r.Context().Done():
			log.Printf("[job %s] client cancelled before queue", jobID)
			http.Error(w, "client cancelled", http.StatusRequestTimeout)
		default:
			log.Printf("[job %s] queue full, rejecting", jobID)
			http.Error(w, "server busy, try again later", http.StatusServiceUnavailable)
		}
	})

	// Simple health check
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})

	server := &http.Server{
		Addr:         ":9000",
		Handler:      mux,
		ReadTimeout:  0, // allow long-lived streaming
		WriteTimeout: 0, // allow long-lived streaming
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Gateway listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("ListenAndServe: %v", err)
	}
}

func worker(idx int, backend string, jobs <-chan *Job) {
	client := &http.Client{
		Timeout: 0, // streaming; no fixed timeout
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	log.Printf("[worker %d] using backend %s", idx, backend)

	for job := range jobs {
		start := time.Now()
		log.Printf("[job %s][worker %d] dispatching to %s %s", job.id, idx, backend, job.r.URL.Path)

		if err := handleJob(client, backend, job); err != nil {
			log.Printf("[job %s][worker %d] error: %v", job.id, idx, err)
		} else {
			log.Printf("[job %s][worker %d] completed in %s", job.id, idx, time.Since(start))
		}

		close(job.done)
	}
}

func handleJob(client *http.Client, backend string, job *Job) error {
	ctx := job.r.Context()
	backendURL, _ := url.Parse(backend)

	// Build target URL: backend base + original path + query
	target := *backendURL // copy
	target.Path = singleJoin(backendURL.Path, job.r.URL.Path)
	target.RawQuery = job.r.URL.RawQuery

	var body io.Reader
	if job.r.Method == http.MethodPost {
		body = job.r.Body
	}

	req, err := http.NewRequestWithContext(ctx, job.r.Method, target.String(), body)
	if err != nil {
		http.Error(job.w, "failed to create upstream request", http.StatusInternalServerError)
		return err
	}

	// Copy headers
	req.Header = job.r.Header.Clone()

	log.Printf("[job %s] -> %s %s", job.id, req.Method, target.String())

	resp, err := client.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			http.Error(job.w, "client cancelled", http.StatusRequestTimeout)
		default:
			http.Error(job.w, "upstream error", http.StatusBadGateway)
		}
		return err
	}
	defer resp.Body.Close()

	// Copy status and headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			job.w.Header().Add(k, v)
		}
	}
	job.w.WriteHeader(resp.StatusCode)

	// Stream body
	_, err = io.Copy(job.w, resp.Body)
	if err != nil {
		log.Printf("[job %s] streaming error: %v", job.id, err)
		return err
	}

	return nil
}

func singleJoin(basePath, subPath string) string {
	if basePath == "" || basePath == "/" {
		return subPath
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(subPath, "/")
}

// randomSuffix is just for log-friendly IDs, not cryptographic.
func randomSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		b[i] = letters[int(now+int64(i))%len(letters)]
	}
	return string(b)
}

