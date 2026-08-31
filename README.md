# chat-style-image-api-bridge

A lightweight compatibility bridge for converting OpenAI Images API requests to chat-style image generation APIs, including NewAPI Gemini image models.

## Supported routes

- `POST /v1/images/generations` (JSON)
- `POST /v1/images/edits` (`multipart/form-data`, supports repeated `image` fields)

Both routes are converted to `POST {UPSTREAM_BASE_URL}/v1/chat/completions`. The upstream response is passed through unchanged.

## Run

```bash
cp .env.example .env
# Set UPSTREAM_BASE_URL and UPSTREAM_API_KEY.
go run ./cmd/server
```

Then configure the fixed client to use this bridge as its OpenAI-compatible base URL:

```text
http://your-server:8080/v1
```

## Request conversion

### Generations

- `prompt` -> `messages[0].content`
- `model` -> `model`
- `size` -> `extra_body.imageConfig.aspectRatio`

### Edits

- Every uploaded `image` file becomes an `image_url` content part using a base64 data URL.
- Multiple images are preserved in upload order.
- `prompt` becomes the first text content part.
- `mask` is currently ignored because chat-style image APIs do not share a portable mask format.

## Configuration

| Variable | Required | Description |
| --- | --- | --- |
| `UPSTREAM_BASE_URL` | Yes | Upstream root URL, with or without `/v1`. |
| `UPSTREAM_API_KEY` | No | Used when the incoming request has no Bearer token. |
| `LISTEN_ADDR` | No | Bind address; defaults to `:8080`. |
| `MAX_MULTIPART_MEMORY` | No | Multipart parse memory limit in bytes; defaults to `33554432`. |

Incoming `Authorization` is forwarded as-is. If it is missing, the bridge uses `UPSTREAM_API_KEY`.