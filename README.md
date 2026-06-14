# Qoder Proxy (Go Edition)

[![CI/CD](https://github.com/InoriHimea/qoder-proxy-go/actions/workflows/release.yml/badge.svg)](https://github.com/InoriHimea/qoder-proxy-go/actions/workflows/release.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/InoriHimea/qoder-proxy-go)](https://golang.org/doc/go1.22)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A high-performance, completely rewritten local proxy for the [Qoder CLI](https://qoder.ai/) and Qoder CN CLI. It seamlessly converts Qoder's proprietary command-line interface into standard **OpenAI** and **Anthropic** compatible REST APIs.

By leveraging Go's `fasthttp` and concurrent I/O streams, this version entirely eliminates the `JavaScript heap out of memory` (OOM) and `spawn E2BIG` crashes found in previous Node.js implementations, enabling it to handle massive context windows with zero overhead.

## ✨ Key Features

- **🚀 Ultra High Performance (Zero OOM)**: Prompts are streamed directly into the CLI process via standard input (`stdin`), bypassing OS argument limits (E2BIG) and internal 256KB attachment caps.
- **🔄 Dual Protocol Support**: Natively supports both `OpenAI` (`/v1/chat/completions`) and `Anthropic` (`/v1/messages`) APIs, including real-time SSE streaming.
- **⚙️ Dynamic Web Dashboard**: An intuitive Web UI (`/dashboard/`) to update your Personal Access Token, toggle between International/CN backends, and manage custom models in real-time—**no container restarts required**.
- **🛠️ Function Calling (Tool Use)**: Automatically intercepts, parses, and executes AI tool calls (e.g., `<tool_code>`), feeding results back to the model in an autonomous loop.
- **📊 Local Usage Tracking**: Built-in, privacy-focused usage tracking to monitor your token counts and request frequency by model locally.
- **🛡️ Secure Redaction**: Automatically redacts sensitive `sk-...` tokens from standard error and logs.

## 📦 Installation

### Option 1: Docker (Recommended)

The easiest way to run the proxy is using the pre-built Docker image hosted on GitHub Container Registry (GHCR).

```bash
docker run -d --name qoder-proxy-go \
  -p 3000:3000 \
  -v $(pwd)/data:/app/data \
  -e DASHBOARD_PASSWORD=your_secure_password \
  ghcr.io/inorihimea/qoder-proxy-go:latest
```

*Note: Mounting the `/app/data` volume ensures your UI-configured tokens and models survive container updates.*

### Option 2: Standalone Binary

If you already have `qodercli` or `qoderclicn` installed globally via npm, you can run the proxy natively.

1. Download the latest binary for your OS (Windows, macOS, Linux) from the [Releases page](https://github.com/InoriHimea/qoder-proxy-go/releases).
2. Make it executable (Linux/macOS): `chmod +x qoder-proxy-linux-amd64`
3. Run it:
   ```bash
   ./qoder-proxy-linux-amd64
   ```

## 🛠️ Configuration

While you *can* use environment variables, it is highly recommended to configure the proxy via the Web Dashboard.

1. Open your browser and navigate to `http://localhost:3000/dashboard/`.
2. Log in using your `DASHBOARD_PASSWORD`.
3. Go to the **Settings** tab.
4. Set your **Backend Type** (`Qoder International` or `Qoder CN`).
5. Paste your **Personal Access Token**.
6. (Optional) Add custom reasoning models to the registry.

### Environment Variables (Fallbacks)
- `PORT`: Port to bind the server to (default: `3000`).
- `DASHBOARD_PASSWORD`: Password to protect the Web UI (default: random hash).
- `CLI_BACKEND`: `global` for `qodercli`, `cn` for `qoderclicn`.
- `QODER_PERSONAL_ACCESS_TOKEN`: Your auth token.
- `QODERCN_PERSONAL_ACCESS_TOKEN`: Your auth token for the CN backend.

## 🌐 API Endpoints

### OpenAI Compatible
- **`GET /v1/models`**: List all available models (from your dynamic configuration).
- **`POST /v1/chat/completions`**: Standard chat completions endpoint. Supports `stream: true`.
- **`POST /v/chat`**: Alias for chat completions.
- **`POST /responses`**: Codex-style completion endpoint.
- **`POST /v1/responses`**: Alias for Codex-style completions.

### Anthropic Compatible
- **`POST /v1/messages`**: Claude-compatible messages endpoint. Supports `stream: true`.
- **`POST /v1/message`**: Alias for messages endpoint.

### Utility & Dashboard
- **`GET /health`**: Returns HTTP 200 OK if the server is running.
- **`GET /usage/local`**: Returns internal usage tracking JSON.
- **`POST /usage/reset-local`**: Resets the local usage tracking statistics.

## 🤝 Contributing

Contributions, issues, and feature requests are welcome! Feel free to check the [issues page](https://github.com/InoriHimea/qoder-proxy-go/issues).

## 📄 License

This project is licensed under the [MIT License](LICENSE).
