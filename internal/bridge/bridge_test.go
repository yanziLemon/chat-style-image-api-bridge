package bridge

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func testBridge(t *testing.T, upstream http.Handler) (*ginTestServer, *httptest.Server) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	cfg := Config{
		UpstreamBaseURL:    upstreamServer.URL,
		UpstreamAPIKey:     "Bearer test-upstream",
		ListenAddr:         ":0",
		MaxMultipartMemory: 1 << 20,
		ModelPrefixes:      []string{"gemini"},
	}
	return newGinTestServer(New(cfg)), upstreamServer
}

type ginTestServer struct {
	handler http.Handler
}

func newGinTestServer(b *Bridge) *ginTestServer {
	// Keep test setup independent from the executable's router configuration.
	return &ginTestServer{handler: handlerForBridge(b)}
}

func handlerForBridge(b *Bridge) http.Handler {
	r := gin.New()
	b.Register(r)
	return r
}

func TestGenerationGeminiConvertsToChat(t *testing.T) {
	var gotPath string
	var got chatRequest
	bridge, upstream := testBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":123,"choices":[{"message":{"content":"data:image/png;base64,aW1hZ2U="}}]}`)
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"cat","size":"1792x1024"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	bridge.handler.ServeHTTP(w, req)

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if got.Model != "gemini-2.5-flash-image" || len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != `{"imageConfig":{"aspectRatio":"16:9"}}` || got.Messages[1].Role != "user" || got.Messages[1].Content != "cat" {
		t.Fatalf("unexpected chat request: %+v", got)
	}
	if got.ExtraBody["imageConfig"].(map[string]any)["aspectRatio"] != "16:9" {
		t.Fatalf("missing aspect ratio: %+v", got.ExtraBody)
	}
	var response imageGenerationResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if w.Code != http.StatusOK || response.Created != 123 || len(response.Data) != 1 || response.Data[0].B64JSON != "aW1hZ2U=" {
		t.Fatalf("response = %d %+v", w.Code, response)
	}
}

func TestGenerationGeminiConvertsChatImageURLToBase64(t *testing.T) {
	var imageURL string
	bridge, upstream := testBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/generated.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("image-bytes"))
			return
		}
		_, _ = io.WriteString(w, `{"created":456,"choices":[{"message":{"content":"![image](`+imageURL+`)"}}]}`)
	}))
	defer upstream.Close()
	imageURL = upstream.URL + "/generated.png"

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gemini-image","prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	bridge.handler.ServeHTTP(w, req)

	var response imageGenerationResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if w.Code != http.StatusOK || response.Created != 456 || len(response.Data) != 1 || response.Data[0].B64JSON != "aW1hZ2UtYnl0ZXM=" {
		t.Fatalf("response = %d %+v", w.Code, response)
	}
}

type multipartImage struct{ field, filename, contentType, data string }

func multipartBody(t *testing.T, model, prompt string, images ...multipartImage) (*strings.Reader, string) {
	t.Helper()
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("prompt", prompt)
	_ = mw.WriteField("size", "1024x1024")
	for _, image := range images {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="`+image.field+`"; filename="`+image.filename+`"`)
		h.Set("Content-Type", image.contentType)
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(image.data))
	}
	_ = mw.Close()
	return strings.NewReader(body.String()), mw.FormDataContentType()
}

func TestEditGeminiConvertsMultipleImages(t *testing.T) {
	var got chatRequest
	bridge, upstream := testBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		_, _ = io.WriteString(w, `{"created":789,"choices":[{"message":{"content":"data:image/jpeg;base64,aW1hZ2U="}}]}`)
	}))
	defer upstream.Close()
	body, contentType := multipartBody(t, "gemini-image", "combine", multipartImage{"image", "a.png", "image/png", "first"}, multipartImage{"image", "b.jpg", "image/jpeg", "second"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	bridge.handler.ServeHTTP(w, req)
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != `{"imageConfig":{"aspectRatio":"1:1"}}` || got.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	parts := got.Messages[1].Content.([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	if gotPath := got.Model; gotPath != "gemini-image" {
		t.Fatalf("model = %q", gotPath)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var response imageGenerationResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Created != 789 || len(response.Data) != 1 || response.Data[0].B64JSON != "aW1hZ2U=" {
		t.Fatalf("response = %+v", response)
	}
}

func TestNonGeminiIsPassedThroughUnchanged(t *testing.T) {
	var gotPath, gotBody, gotAuth string
	bridge, upstream := testBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `passthrough`)
	}))
	defer upstream.Close()
	body := `{"model":"dall-e-3","prompt":"cat","size":"1024x1024"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	bridge.handler.ServeHTTP(w, req)
	if gotPath != "/v1/images/generations" || gotBody != body {
		t.Fatalf("not unchanged: path=%q body=%q", gotPath, gotBody)
	}
	if gotAuth != "Bearer test-upstream" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if w.Code != http.StatusAccepted || w.Body.String() != "passthrough" {
		t.Fatalf("response = %d %s", w.Code, w.Body.String())
	}
}
