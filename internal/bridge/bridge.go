package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type Config struct {
	UpstreamBaseURL    string
	UpstreamAPIKey     string
	ListenAddr         string
	MaxMultipartMemory int64
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

type chatCompletionResponse struct {
	Created int64 `json:"created"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type imageGenerationResponse struct {
	Created int64                  `json:"created"`
	Data    []imageGenerationDatum `json:"data"`
}

type imageGenerationDatum struct {
	B64JSON string `json:"b64_json"`
}

var (
	dataImagePattern = regexp.MustCompile(`data:image/[^;"\s]+;base64,([^"\)\s]+)`)
	imageURLPattern  = regexp.MustCompile(`https?://[^"\s\)]+`)
)

func New(cfg Config) *Bridge {
	return &Bridge{cfg: cfg, httpClient: &http.Client{}}
}

func (b *Bridge) Register(r *gin.Engine) {
	r.POST("/v1/images/generations", b.handleGenerations)
	r.POST("/v1/images/edits", b.handleEdits)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
}

func (b *Bridge) handleGenerations(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
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
	body, err := io.ReadAll(c.Request.Body)
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
	messages := []chatMessage{{Role: "user", Content: content}}
	req := chatRequest{Model: model, Stream: false, Messages: messages}
	if ratio := sizeToAspectRatio(size); ratio != "" {
		imageConfig := map[string]string{"aspectRatio": ratio}
		configJSON, _ := json.Marshal(map[string]map[string]string{"imageConfig": imageConfig})
		req.Messages = append([]chatMessage{{Role: "system", Content: string(configJSON)}}, req.Messages...)
		req.ExtraBody = map[string]any{"imageConfig": imageConfig}
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
	endpoint, err := upstreamEndpoint(b.cfg.UpstreamBaseURL, "/v1/chat/completions")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "configuration_error"}})
		return
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "bridge_error"}})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	b.setAuthorization(c, req)
	resp, err := b.httpClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		b.copyUpstreamResponse(c, resp)
		return
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	imageBase64, err := b.imageBase64FromChatResponse(c.Request.Context(), responseBody)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	var chatResponse chatCompletionResponse
	if err := json.Unmarshal(responseBody, &chatResponse); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "invalid chat response from upstream", "type": "upstream_error"}})
		return
	}
	c.JSON(http.StatusOK, imageGenerationResponse{
		Created: chatResponse.Created,
		Data:    []imageGenerationDatum{{B64JSON: imageBase64}},
	})
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
	b.setAuthorization(c, req)
	resp, err := b.httpClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()
	b.copyUpstreamResponse(c, resp)
}

func (b *Bridge) setAuthorization(c *gin.Context, req *http.Request) {
	auth := c.GetHeader("Authorization")
	if auth == "" && b.cfg.UpstreamAPIKey != "" {
		auth = b.cfg.UpstreamAPIKey
		if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			auth = "Bearer " + auth
		}
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
}

func (b *Bridge) copyUpstreamResponse(c *gin.Context, resp *http.Response) {
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

func (b *Bridge) imageBase64FromChatResponse(ctx context.Context, responseBody []byte) (string, error) {
	var chatResponse chatCompletionResponse
	if err := json.Unmarshal(responseBody, &chatResponse); err != nil {
		return "", fmt.Errorf("invalid chat response from upstream")
	}
	if len(chatResponse.Choices) == 0 {
		return "", fmt.Errorf("upstream chat response did not include a choice")
	}
	var content string
	if err := json.Unmarshal(chatResponse.Choices[0].Message.Content, &content); err != nil || strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("upstream chat response did not include text image content")
	}
	if match := dataImagePattern.FindStringSubmatch(content); len(match) == 2 {
		if _, err := base64.StdEncoding.DecodeString(match[1]); err != nil {
			return "", fmt.Errorf("upstream chat response contained invalid image base64")
		}
		return match[1], nil
	}
	imageURL := imageURLPattern.FindString(content)
	if imageURL == "" {
		return "", fmt.Errorf("upstream chat response did not include an image URL")
	}
	return b.downloadImageBase64(ctx, imageURL)
}

func (b *Bridge) downloadImageBase64(ctx context.Context, imageURL string) (string, error) {
	parsed, err := url.ParseRequestURI(imageURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("upstream chat response contained an invalid image URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("image download returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
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
	return Config{UpstreamBaseURL: os.Getenv("UPSTREAM_BASE_URL"), UpstreamAPIKey: os.Getenv("UPSTREAM_API_KEY"), ListenAddr: addr, MaxMultipartMemory: maxMemory, ModelPrefixes: prefixes}
}
