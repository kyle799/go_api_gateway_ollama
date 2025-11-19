package main

import (
	"bytes"
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

// NGINX uses 499 for "client closed request".
// Go's net/http does not define this status constant.
const StatusClientClosedRequest = 499

type Job struct {
	id         string
	method     string
	path       string
	rawQuery   string
	headers    http.Header
	body       []byte
	enqueuedAt time.Time
	w          http.ResponseWriter
	r          *http.Request
	done       chan struct{}
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
	maxQueueWait := 0 * time.Second // unlimited queue wait by default

	if qs := os.Getenv("GATEWAY_QUEUE_SIZE"); qs != "" {
		if n, err := strconv.Atoi(qs); err == nil && n > 0 {
			queueSize = n
		}
	}

	if s := os.Getenv("GATEWAY_MAX_QUEUE_WAIT_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxQueueWait = time.Duration(n) * time.Second
		}
	}

	log.Printf("Starting gateway with %d backends, queue size %d, maxQueueWait=%s",
		len(backendURLs), queueSize, maxQueueWait)

	jobCh := make(chan *Job, queueSize)

	// One worker per backend => one active request per backend
	for i, be := range backendURLs {
		go worker(i, be, jobCh, maxQueueWait)
	}

	mux := http.NewServeMux()

	// Main API entrypoint for OpenWebUI/Ollama-compatible endpoints.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		jobID := time.Now().Format("20060102-150405.000000") + "-" + randomSuffix(6)
		log.Printf("[job %s] received %s %s from %s", jobID, r.Method, r.URL.Path, r.RemoteAddr)

		switch r.Method {
		case http.MethodPost, http.MethodGet, http.MethodOptions:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read body up front so we do not block the client
		var body []byte
		if r.Body != nil {
			defer r.Body.Close()
			// You can wrap this in http.MaxBytesReader if you want a hard limit.
			b, err := io.ReadAll(r.Body)
			if err != nil {
				log.Printf("[job %s] error reading body: %v", jobID, err)
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			body = b
		}

		job := &Job{
			id:         jobID,
			method:     r.Method,
			path:       r.URL.Path,
			rawQuery:   r.URL.RawQuery,
			headers:    r.Header.Clone(),
			body:       body,
			enqueuedAt: time.Now(),
			w:          w,
			r:          r,
			done:       make(chan struct{}),
		}

		// Try to enqueue
		select {
		case jobCh <- job:
			log.Printf("[job %s] enqueued", jobID)
			<-job.done
			log.Printf("[job %s] handler completed", jobID)

		case <-r.Context().Done():
			log.Printf("[job %s] client cancelled before enqueue", jobID)
			http.Error(w, "client cancelled", StatusClientClosedRequest)

		default:
			log.Printf("[job %s] queue full, rejecting", jobID)
			http.Error(w, "server busy, try again later", http.StatusServiceUnavailable)
		}
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})

	server := &http.Server{
		Addr:         ":9000",
		Handler:      mux,
		ReadTimeout:  0, // allow long-lived requests
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
		Timeout: 0, // do not hard-timeout; upstream/ctx controls this
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          0,
			MaxIdleConnsPerHost:   0,
			IdleConnTimeout:       0,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	log.Printf("[worker %d] using backend %s", idx, backend)

	for job := range jobs {
		start := time.Now()
		waitTime := start.Sub(job.enqueuedAt)
		log.Printf("[job %s][worker %d] dequeued after %s", job.id, idx, waitTime)

		// If client cancelled while in queue, do not bother upstream.
		select {
		case <-job.r.Context().Done():
			log.Printf("[job %s][worker %d] client cancelled while queued", job.id, idx)
			safeError(job.w, "client cancelled", StatusClientClosedRequest)
			close(job.done)
			continue
		default:
		}

		// Optional: enforce max queue wait
		if maxQueueWait > 0 && waitTime > maxQueueWait {
			log.Printf("[job %s][worker %d] exceeded max queue wait (%s > %s)",
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
	target.Path = singleJoin(backendURL.Path, job.path)
	target.RawQuery = job.rawQuery

	req, err := http.NewRequestWithContext(ctx, job.method, target.String(), bytes.NewReader(job.body))
	if err != nil {
		safeError(job.w, "failed to create upstream request", http.StatusInternalServerError)
		return err
	}

	// copy headers and disable keep-alive both directions
	req.Header = job.headers.Clone()
	req.Close = true

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

	// Response headers; force connection close to client
	for k, vv := range resp.Header {
		for _, v := range vv {
			job.w.Header().Add(k, v)
		}
	}
	job.w.Header().Set("Connection", "close")
	job.w.WriteHeader(resp.StatusCode)

	// Stream body straight through
	_, err = io.Copy(job.w, resp.Body)
	if err != nil {
		log.Printf("[job %s] streaming error: %v", job.id, err)
	}
	return err
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

func safeError(w http.ResponseWriter, msg string, code int) {
	defer func() { _ = recover() }()
	http.Error(w, msg, code)
}

