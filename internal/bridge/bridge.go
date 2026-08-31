package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type Config struct {
	UpstreamBaseURL    string
	UpstreamAPIKey     string
	ListenAddr         string
	MaxMultipartMemory int64`r`n	MaxRequestBody int64
	ModelPrefixes      []string
}

type Bridge struct {
	cfg        Config
	httpClient *http.Client
}

type generationRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
	User           string `json:"user"`
}

type chatRequest struct {
	Model     string         `json:"model"`
	Stream    bool           `json:"stream"`
	Messages  []chatMessage  `json:"messages"`
	ExtraBody map[string]any `json:"extra_body,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

func New(cfg Config) *Bridge {
	return &Bridge{cfg: cfg, httpClient: &http.Client{}}
}

func (b *Bridge) Register(r *gin.Engine) {
	r.POST("/v1/images/generations", b.handleGenerations)
	r.POST("/v1/images/edits", b.handleEdits)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
}

func (b *Bridge) handleGenerations(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, b.cfg.MaxRequestBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	var req generationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	if !b.handlesModel(req.Model) {
		b.forwardRaw(c, "/v1/images/generations", body, c.GetHeader("Content-Type"))
		return
	}
	if strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model and prompt are required", "type": "invalid_request_error"}})
		return
	}
	b.forwardChat(c, buildChatRequest(req.Model, req.Prompt, nil, req.Size))
}

func (b *Bridge) handleEdits(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, b.cfg.MaxRequestBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	if err := c.Request.ParseMultipartForm(b.cfg.MaxMultipartMemory); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("invalid multipart form: %v", err), "type": "invalid_request_error"}})
		return
	}
	model := strings.TrimSpace(c.PostForm("model"))
	if !b.handlesModel(model) {
		b.forwardRaw(c, "/v1/images/edits", body, c.GetHeader("Content-Type"))
		return
	}
	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if model == "" || prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model and prompt are required", "type": "invalid_request_error"}})
		return
	}
	headers := c.Request.MultipartForm.File["image"]
	if len(headers) == 0 {
		headers = c.Request.MultipartForm.File["images"]
	}
	if len(headers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "at least one image file is required", "type": "invalid_request_error"}})
		return
	}
	parts := make([]contentPart, 0, len(headers)+1)
	parts = append(parts, contentPart{Type: "text", Text: prompt})
	for _, fh := range headers {
		file, err := fh.Open()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
			return
		}
		data, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			if readErr != nil {
				err = readErr
			} else {
				err = closeErr
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
			return
		}
		contentType := fh.Header.Get("Content-Type")
		if contentType == "" || contentType == "application/octet-stream" {
			contentType = http.DetectContentType(data)
		}
		if _, _, err := mime.ParseMediaType(contentType); err != nil {
			contentType = "application/octet-stream"
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURLPart{URL: "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)}})
	}
	b.forwardChat(c, buildChatRequest(model, "", parts, c.PostForm("size")))
}
func (b *Bridge) handlesModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || len(b.cfg.ModelPrefixes) == 0 {
		return false
	}
	for _, prefix := range b.cfg.ModelPrefixes {
		if strings.HasPrefix(model, strings.ToLower(strings.TrimSpace(prefix))) {
			return true
		}
	}
	return false
}
func buildChatRequest(model, prompt string, parts []contentPart, size string) chatRequest {
	var content any = prompt
	if parts != nil {
		content = parts
	}
	req := chatRequest{Model: model, Stream: false, Messages: []chatMessage{{Role: "user", Content: content}}}
	if ratio := sizeToAspectRatio(size); ratio != "" {
		req.ExtraBody = map[string]any{"imageConfig": map[string]string{"aspectRatio": ratio}}
	}
	return req
}

func sizeToAspectRatio(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1024x1024", "square":
		return "1:1"
	case "1024x1536", "portrait":
		return "2:3"
	case "1536x1024", "landscape":
		return "3:2"
	case "1024x1792":
		return "9:16"
	case "1792x1024":
		return "16:9"
	default:
		return ""
	}
}

func (b *Bridge) forwardChat(c *gin.Context, payload chatRequest) {
	data, err := json.Marshal(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "bridge_error"}})
		return
	}
	b.forwardRaw(c, "/v1/chat/completions", data, "application/json")
}

func (b *Bridge) forwardRaw(c *gin.Context, route string, data []byte, contentType string) {
	endpoint, err := upstreamEndpoint(b.cfg.UpstreamBaseURL, route)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "configuration_error"}})
		return
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "bridge_error"}})
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	auth := c.GetHeader("Authorization")
	if auth == "" && b.cfg.UpstreamAPIKey != "" {
		auth = "Bearer " + b.cfg.UpstreamAPIKey
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}
func upstreamEndpoint(base, route string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", fmt.Errorf("UPSTREAM_BASE_URL is required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid UPSTREAM_BASE_URL")
	}
	if !strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path = path.Join(parsed.Path, "v1")
	}
	route = strings.TrimPrefix(route, "/")
	route = strings.TrimPrefix(route, "v1/")
	parsed.Path = path.Join(parsed.Path, route)
	return parsed.String(), nil
}

func parsePrefixes(value string) []string {
	var prefixes []string
	for _, item := range strings.Split(value, ",") {
		if prefix := strings.TrimSpace(item); prefix != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}
func LoadConfig() Config {
	maxMemory := int64(32 << 20)
	if value, err := strconv.ParseInt(os.Getenv("MAX_MULTIPART_MEMORY"), 10, 64); err == nil && value > 0 {
		maxMemory = value
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	prefixes := parsePrefixes(os.Getenv("MODEL_PREFIXES"))
	if len(prefixes) == 0 {
		prefixes = []string{"gemini"}
	}
	return Config{UpstreamBaseURL: os.Getenv("UPSTREAM_BASE_URL"), UpstreamAPIKey: os.Getenv("UPSTREAM_API_KEY"), ListenAddr: addr, MaxMultipartMemory: maxMemory, MaxRequestBody: maxMemory * 4, ModelPrefixes: prefixes}
}
