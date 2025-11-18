package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Job struct {
	w          http.ResponseWriter
	r          *http.Request
	id         string
	enqueuedAt time.Time
	done       chan struct{}
}
const StatusClientClosedRequest = 499

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
		if n, err := strconv.Atoi(qs); err == nil && n > 0 {
			queueSize = n
		}
	}

	maxQueueWait := 0 * time.Second
	if s := os.Getenv("GATEWAY_MAX_QUEUE_WAIT_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxQueueWait = time.Duration(n) * time.Second
		}
	}

	log.Printf("Starting gateway with %d backends, queue size %d, maxQueueWait=%s",
		len(backendURLs), queueSize, maxQueueWait)

	jobCh := make(chan *Job, queueSize)

	// Start one worker per backend → max 1 concurrent job per backend
	for i, be := range backendURLs {
		go worker(i, be, jobCh, maxQueueWait)
	}

	mux := http.NewServeMux()

	// Main Ollama-compatible API handler
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		jobID := time.Now().Format("20060102-150405.000000") + "-" + randomSuffix(6)
		log.Printf("[job %s] received %s %s from %s", jobID, r.Method, r.URL.Path, r.RemoteAddr)

		switch r.Method {
		case http.MethodPost, http.MethodGet, http.MethodOptions:
			// ok
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		job := &Job{
			w:          w,
			r:          r,
			id:         jobID,
			enqueuedAt: time.Now(),
			done:       make(chan struct{}),
		}

		// Try to enqueue
		select {
		case jobCh <- job:
			log.Printf("[job %s] enqueued", jobID)
			// Wait until worker finishes (or marks it cancelled)
			<-job.done
			log.Printf("[job %s] handler finished", jobID)

		case <-r.Context().Done():
			log.Printf("[job %s] client cancelled before enqueue: %v", jobID, r.Context().Err())
			http.Error(w, "client cancelled", StatusClientClosedRequest)

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

func worker(idx int, backend string, jobs <-chan *Job, maxQueueWait time.Duration) {
	client := &http.Client{
		Timeout: 0,
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
		waitTime := start.Sub(job.enqueuedAt)
		log.Printf("[job %s][worker %d] dequeued after %s", job.id, idx, waitTime)

		// If the client already cancelled while in queue, skip
		select {
		case <-job.r.Context().Done():
			log.Printf("[job %s][worker %d] client cancelled while queued: %v", job.id, idx, job.r.Context().Err())
			// Best-effort error; client is gone anyway
			safeError(job.w, "client cancelled", StatusClientClosedRequest)
			close(job.done)
			continue
		default:
		}

		// Optional: enforce max queue wait
		if maxQueueWait > 0 && waitTime > maxQueueWait {
			log.Printf("[job %s][worker %d] exceeded max queue wait (%s > %s), rejecting",
				job.id, idx, waitTime, maxQueueWait)
			safeError(job.w, "server busy, try again later", http.StatusServiceUnavailable)
			close(job.done)
			continue
		}

		err := handleJob(client, backend, job)
		if err != nil {
			log.Printf("[job %s][worker %d] error: %v", job.id, idx, err)
		} else {
			log.Printf("[job %s][worker %d] completed in %s (incl. wait %s)",
				job.id, idx, time.Since(start), waitTime)
		}
		close(job.done)
	}
}

func handleJob(client *http.Client, backend string, job *Job) error {
	ctx := job.r.Context()
	backendURL, _ := url.Parse(backend)

	target := *backendURL
	target.Path = singleJoin(backendURL.Path, job.r.URL.Path)
	target.RawQuery = job.r.URL.RawQuery

	// New upstream request with same method and body, pointed at backend
	req, err := http.NewRequestWithContext(ctx, job.r.Method, target.String(), job.r.Body)
	if err != nil {
		safeError(job.w, "failed to create upstream request", http.StatusInternalServerError)
		return err
	}

	// Copy headers through
	req.Header = job.r.Header.Clone()

	log.Printf("[job %s] -> %s %s", job.id, req.Method, target.String())

	resp, err := client.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			safeError(job.w, "client cancelled", StatusClientClosedRequest)
		default:
			safeError(job.w, "upstream error", http.StatusBadGateway)
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

	// Stream body directly
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

func randomSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		b[i] = letters[int(now+int64(i))%len(letters)]
	}
	return string(b)
}

// safeError tries to send an error to the client but ignores write failures.
func safeError(w http.ResponseWriter, msg string, code int) {
	defer func() {
		_ = recover()
	}()
	http.Error(w, msg, code)
}

