# go_api_gateway_ollama

A lightweight Go‑based API gateway that forwards requests to one or more Ollama‑compatible back‑ends.  
It is designed to work behind Nginx (or any other reverse proxy) and provides a simple health‑check endpoint.

---

## Table of Contents

- [Overview](#overview)
- [Features](#features)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Configuration](#configuration)
- [Running](#running)
- [API](#api)
- [Health Check](#health-check)
- [Example Nginx Configuration](#example-nginx-configuration)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

---

## Overview

`go_api_gateway_ollama` sits in front of one or more Ollama servers and distributes incoming requests in a round‑robin fashion.  
Each worker has a single active request to its corresponding back‑end, preventing overload and allowing a simple queueing mechanism when all workers are busy.

---

## Features

- **Dynamic back‑end list** – set via `GATEWAY_BACKENDS`.
- **Request queue** – configurable size (`GATEWAY_QUEUE_SIZE`) and maximum wait (`GATEWAY_MAX_QUEUE_WAIT_SECONDS`).
- **Graceful handling of client cancellations** – uses status code 499 (client closed request) for aborted connections.
- **Health‑check endpoint** – `/healthz` forwards to the back‑end’s health‑check.

---

## Prerequisites

- Go 1.22 or newer
- A running Ollama instance(s) (any compatible API)

---

## Installation

```bash
# Clone the repository
git clone https://github.com/kyle799/go_api_gateway_ollama.git
cd go_api_gateway_ollama

# Build the binary
go build -o gateway main.go
```

---

## Configuration

The gateway relies on environment variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `GATEWAY_BACKENDS` | Comma‑separated list of back‑end URLs (required) | `http://127.0.0.1:11434,http://127.0.0.1:11435` |
| `GATEWAY_QUEUE_SIZE` | Maximum number of queued jobs (default 100) | `200` |
| `GATEWAY_MAX_QUEUE_WAIT_SECONDS` | Max time (seconds) a job waits in queue; 0 = unlimited (default) | `30` |

> **Tip**: If any variable is missing or malformed, the gateway will exit with an error.

---

## Running

```bash
# Export the required environment variables
export GATEWAY_BACKENDS="http://127.0.0.1:11434,http://127.0.0.1:11435"
export GATEWAY_QUEUE_SIZE=200
export GATEWAY_MAX_QUEUE_WAIT_SECONDS=30

# Start the gateway
./gateway
```

The service listens on port `80` by default.  To change the listening port, edit the `main.go` source or set `GATEWAY_LISTEN_ADDR` if the code is extended to support it.

---

## API

All API routes that begin with `/api/` are forwarded to the configured back‑ends.  
Supported methods: **GET**, **POST**, **OPTIONS**.

Example request:

```bash
curl -X POST http://localhost/api/prompt \
     -H "Content-Type: application/json" \
     -d '{"model":"llama2","prompt":"Hello world"}'
```

The gateway logs each request with a unique job ID for traceability.

---

## Health Check

The `/healthz` endpoint forwards to the back‑end’s `/healthz`.  It can be used by orchestration tools (e.g., Docker Compose, Kubernetes) to monitor availability.

```bash
curl http://localhost/healthz
```

---

## Example Nginx Configuration

Below is a minimal Nginx config that forwards traffic to the Go gateway.  The snippet is extracted from the repository’s `nginx.conf.exmaple` source [1].

```nginx
server {
    listen 80;
    server_name _;

    client_max_body_size 512M;

    proxy_read_timeout  3600s;
    proxy_connect_timeout 3600s;
    proxy_send_timeout 3600s;
    send_timeout        3600s;

    proxy_buffering off;
    proxy_request_buffering off;
    gzip off;

    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    location /api/ {
        proxy_http_version 1.1;
        proxy_set_header Connection "";

        proxy_pass http://127.0.0.1:9000;
    }

    location /healthz {
        proxy_pass http://127.0.0.1:9000/healthz;
    }
}
```

> Replace `127.0.0.1:9000` with the actual address where the Go gateway is listening.

---

## Development

The core logic is in `main.go` [2].  The code implements:

- Environment variable parsing.
- A worker pool with one goroutine per back‑end.
- A bounded queue for incoming jobs.
- HTTP handlers for `/api/` and `/healthz`.
- Logging of job lifecycle events.

Feel free to extend the worker logic or add new endpoints.

---

## Contributing

Pull requests are welcome.  
Please follow the existing code style and add tests where applicable.

---

## License

MIT © 2024 kyle799

---
