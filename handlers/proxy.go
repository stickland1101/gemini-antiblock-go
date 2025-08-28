package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gemini-antiblock/config"
	"gemini-antiblock/logger"
	"gemini-antiblock/streaming"
)

// ProxyHandler handles proxy requests to Gemini API
type ProxyHandler struct {
	Config      *config.Config
	RateLimiter *RateLimiter
}

// NewProxyHandler creates a new proxy handler
func NewProxyHandler(cfg *config.Config, rateLimiter *RateLimiter) *ProxyHandler {
	return &ProxyHandler{
		Config:      cfg,
		RateLimiter: rateLimiter,
	}
}

// BuildUpstreamHeaders builds headers for upstream requests
func (h *ProxyHandler) BuildUpstreamHeaders(reqHeaders http.Header) http.Header {
	headers := make(http.Header)

	// Copy specific headers
	if auth := reqHeaders.Get("Authorization"); auth != "" {
		headers.Set("Authorization", auth)
	}
	if apiKey := reqHeaders.Get("X-Goog-Api-Key"); apiKey != "" {
		headers.Set("X-Goog-Api-Key", apiKey)
	}
	if contentType := reqHeaders.Get("Content-Type"); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if accept := reqHeaders.Get("Accept"); accept != "" {
		headers.Set("Accept", accept)
	}

	return headers
}

// InjectSystemPrompt injects a system prompt to ensure the [done] token is present.
// It intelligently handles both system_instruction (snake_case) and systemInstruction (camelCase)
// by merging the content of system_instruction into systemInstruction before processing.
// systemInstruction is the officially recommended format.
func (h *ProxyHandler) InjectSystemPrompt(body map[string]interface{}) {
	newSystemPromptPart := map[string]interface{}{
		"text": "IMPORTANT: At the very end of your entire response, you must write the token [done] to signal completion. This is a mandatory technical requirement.",
	}

	// Standardize: If system_instruction exists, merge its content into systemInstruction.
	if snakeVal, snakeExists := body["system_instruction"]; snakeExists {
		// Ensure camelCase map exists
		camelMap, _ := body["systemInstruction"].(map[string]interface{})
		if camelMap == nil {
			camelMap = make(map[string]interface{})
		}

		// Ensure camelCase parts array exists
		camelParts, _ := camelMap["parts"].([]interface{})
		if camelParts == nil {
			camelParts = make([]interface{}, 0)
		}

		// If snake_case is a valid map with its own parts, prepend them to camelCase parts
		if snakeMap, snakeOk := snakeVal.(map[string]interface{}); snakeOk {
			if snakeParts, snakePartsOk := snakeMap["parts"].([]interface{}); snakePartsOk {
				camelParts = append(snakeParts, camelParts...)
			}
		}

		// Update the camelCase field with the merged parts and delete the snake_case one
		camelMap["parts"] = camelParts
		body["systemInstruction"] = camelMap
		delete(body, "system_instruction")
	}

	// --- From this point on, we only need to deal with systemInstruction ---

	// Case 1: systemInstruction field is missing or null. Create it.
	if val, exists := body["systemInstruction"]; !exists || val == nil {
		body["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{newSystemPromptPart},
		}
		return
	}

	instruction, ok := body["systemInstruction"].(map[string]interface{})
	if !ok {
		// The field exists but is of the wrong type. Overwrite it.
		body["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{newSystemPromptPart},
		}
		return
	}

	// Case 2: The instruction field exists, but its 'parts' array is missing, null, or not an array.
	parts, ok := instruction["parts"].([]interface{})
	if !ok {
		instruction["parts"] = []interface{}{newSystemPromptPart}
		return
	}

	// Case 3: The instruction field and its 'parts' array both exist. Append to the existing array.
	instruction["parts"] = append(parts, newSystemPromptPart)
}

// HandleStreamingPost handles streaming POST requests
func (h *ProxyHandler) HandleStreamingPost(w http.ResponseWriter, r *http.Request) {
	urlObj, _ := url.Parse(r.URL.String())
	upstreamURL := h.Config.UpstreamURLBase + urlObj.Path
	if urlObj.RawQuery != "" {
		upstreamURL += "?" + urlObj.RawQuery
	}

	logger.LogInfo("=== NEW STREAMING REQUEST ===")
	logger.LogInfo("Upstream URL:", upstreamURL)
	logger.LogInfo("Request method:", r.Method)
	logger.LogInfo("Content-Type:", r.Header.Get("Content-Type"))

	// Read and parse request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logger.LogError("Failed to read request body:", err)
		JSONError(w, 400, "Failed to read request body", err.Error())
		return
	}

	var requestBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
		logger.LogError("Failed to parse request body:", err)
		JSONError(w, 400, "Invalid JSON in request body", err.Error())
		return
	}

	logger.LogDebug(fmt.Sprintf("Request body size: %d bytes", len(bodyBytes)))

	if contents, ok := requestBody["contents"].([]interface{}); ok {
		if len(contents) == 0 {
			logger.LogError("Request contains empty contents array")
			JSONError(w, 400, "Request must contain at least one message in contents", "empty_contents")
			return
		}
		logger.LogDebug(fmt.Sprintf("Parsed request body with %d messages", len(contents)))
	} else {
		// contents字段不存在或类型错误
		logger.LogError("Request missing or invalid contents field")
		JSONError(w, 400, "Request must contain valid contents field", "missing_contents")
		return
	}

	// === TOKEN LIMIT CHECK START ===
	modelName := extractModelFromPath(r.URL.Path)
	if modelName != "" {
		if maxTokens, ok := h.Config.GeminiModelMaxTokens[modelName]; ok {
			estimatedTokens := estimateTokenCount(requestBody)
			logger.LogDebug(fmt.Sprintf("Model: %s, Max Tokens: %d, Estimated Tokens: %d", modelName, maxTokens, estimatedTokens))
			if estimatedTokens > maxTokens {
				logger.LogError(fmt.Sprintf("Token limit exceeded for model %s. Limit: %d, Estimated: %d", modelName, maxTokens, estimatedTokens))
				JSONError(w, h.Config.TokenLimitExceededCode, h.Config.TokenLimitExceededMessage, "token_limit_exceeded")
				return
			}
		}
	}
	// === TOKEN LIMIT CHECK END ===

	// Inject system prompt
	h.InjectSystemPrompt(requestBody)

	// Log the final request body before sending
	if logger.IsDebugMode() {
		debugRequestBodyBytes, _ := json.Marshal(requestBody)
		logger.LogDebug("Final upstream request body:", string(debugRequestBodyBytes))
	}

	// Create upstream request
	modifiedBodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		logger.LogError("Failed to marshal modified request body:", err)
		JSONError(w, 500, "Internal server error", "Failed to process request body")
		return
	}

	logger.LogInfo("=== MAKING INITIAL REQUEST ===")
	upstreamHeaders := h.BuildUpstreamHeaders(r.Header)

	upstreamReq, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(modifiedBodyBytes))
	if err != nil {
		logger.LogError("Failed to create upstream request:", err)
		JSONError(w, 500, "Internal server error", "Failed to create upstream request")
		return
	}

	upstreamReq.Header = upstreamHeaders

	client := &http.Client{}
	initialResponse, err := client.Do(upstreamReq)
	if err != nil {
		logger.LogError("Failed to make initial request:", err)
		JSONError(w, 502, "Bad Gateway", "Failed to connect to upstream server")
		return
	}

	logger.LogInfo(fmt.Sprintf("Initial response status: %d %s", initialResponse.StatusCode, initialResponse.Status))

	// Initial failure: return standardized error
	if initialResponse.StatusCode != http.StatusOK {
		logger.LogError("=== INITIAL REQUEST FAILED ===")
		logger.LogError("Status:", initialResponse.StatusCode)
		logger.LogError("Status Text:", initialResponse.Status)

		// Read error response
		errorBody, _ := io.ReadAll(initialResponse.Body)
		initialResponse.Body.Close()

		// Try to parse as JSON error
		var errorResp map[string]interface{}
		if json.Unmarshal(errorBody, &errorResp) == nil {
			if errorObj, ok := errorResp["error"].(map[string]interface{}); ok {
				if _, hasStatus := errorObj["status"]; !hasStatus {
					if code, ok := errorObj["code"].(float64); ok {
						errorObj["status"] = StatusToGoogleStatus(int(code))
					}
				}
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(initialResponse.StatusCode)
			json.NewEncoder(w).Encode(errorResp)
			return
		}

		// Fallback to standard error
		message := "Request failed"
		if initialResponse.StatusCode == 429 {
			message = "Resource has been exhausted (e.g. check quota)."
		}
		JSONError(w, initialResponse.StatusCode, message, string(errorBody))
		return
	}

	logger.LogInfo("=== INITIAL REQUEST SUCCESSFUL - STARTING STREAM PROCESSING ===")

	// Set up streaming response
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Additional headers to prevent buffering by proxies
	w.Header().Set("X-Accel-Buffering", "no") // Nginx
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	w.WriteHeader(http.StatusOK)

	// Process stream with retry logic
	err = streaming.ProcessStreamAndRetryInternally(
		h.Config,
		initialResponse.Body,
		w,
		requestBody,
		upstreamURL,
		r.Header,
	)

	if err != nil {
		logger.LogError("=== UNHANDLED EXCEPTION IN STREAM PROCESSOR ===")
		logger.LogError("Exception:", err)
	}

	initialResponse.Body.Close()
	logger.LogInfo("Streaming response completed")
}

// HandleNonStreaming handles non-streaming requests
func (h *ProxyHandler) HandleNonStreaming(w http.ResponseWriter, r *http.Request) {
	urlObj, _ := url.Parse(r.URL.String())
	upstreamURL := h.Config.UpstreamURLBase + urlObj.Path
	if urlObj.RawQuery != "" {
		upstreamURL += "?" + urlObj.RawQuery
	}

	upstreamHeaders := h.BuildUpstreamHeaders(r.Header)

	var body io.Reader
	if r.Method != "GET" && r.Method != "HEAD" {
		body = r.Body
	}

	upstreamReq, err := http.NewRequest(r.Method, upstreamURL, body)
	if err != nil {
		JSONError(w, 500, "Internal server error", "Failed to create upstream request")
		return
	}

	upstreamReq.Header = upstreamHeaders

	client := &http.Client{}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		JSONError(w, 502, "Bad Gateway", "Failed to connect to upstream server")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Handle error response
		errorBody, _ := io.ReadAll(resp.Body)

		var errorResp map[string]interface{}
		if json.Unmarshal(errorBody, &errorResp) == nil {
			if errorObj, ok := errorResp["error"].(map[string]interface{}); ok {
				if _, hasStatus := errorObj["status"]; !hasStatus {
					if code, ok := errorObj["code"].(float64); ok {
						errorObj["status"] = StatusToGoogleStatus(int(code))
					}
				}
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(resp.StatusCode)
			json.NewEncoder(w).Encode(errorResp)
			return
		}

		JSONError(w, resp.StatusCode, resp.Status, string(errorBody))
		return
	}

	// Copy response headers
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ServeHTTP implements the http.Handler interface
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// First, enforce rate limiting if enabled and a key is present.
	if h.Config.EnableRateLimit {
		apiKey := r.Header.Get("X-Goog-Api-Key")
		if apiKey == "" {
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				apiKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		if apiKey != "" {
			logger.LogDebug("Enforcing rate limit for key ending with: ...", apiKey[len(apiKey)-4:])
			h.RateLimiter.Wait(apiKey)
			logger.LogDebug("Rate limit check passed for key.")
		}
	}

	logger.LogInfo("=== WORKER REQUEST ===")
	logger.LogInfo("Method:", r.Method)
	logger.LogInfo("URL:", r.URL.String())
	logger.LogInfo("User-Agent:", r.Header.Get("User-Agent"))
	logger.LogInfo("X-Forwarded-For:", r.Header.Get("X-Forwarded-For"))

	if r.Method == "OPTIONS" {
		logger.LogDebug("Handling CORS preflight request")
		HandleCORS(w, r)
		return
	}

	// Determine if this is a streaming request
	isStream := strings.Contains(strings.ToLower(r.URL.Path), "stream") ||
		strings.Contains(strings.ToLower(r.URL.Path), "sse") ||
		r.URL.Query().Get("alt") == "sse"

	logger.LogInfo("Detected streaming request:", isStream)

	if r.Method == "POST" && isStream {
		h.HandleStreamingPost(w, r)
		return
	}

	h.HandleNonStreaming(w, r)
}

// estimateTokenCount estimates the number of tokens in the request.
// NOTE: This is a simple word-count based estimation and not a precise tokenizer.
func estimateTokenCount(body map[string]interface{}) int {
	count := 0
	if contents, ok := body["contents"].([]interface{}); ok {
		for _, content := range contents {
			if contentMap, ok := content.(map[string]interface{}); ok {
				if parts, ok := contentMap["parts"].([]interface{}); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							if text, ok := partMap["text"].(string); ok {
								count += len(strings.Fields(text))
							}
						}
					}
				}
			}
		}
	}
	return count
}

// extractModelFromPath extracts the model name from the request URL path.
// e.g., /v1beta/models/gemini-1.5-pro-latest:generateContent -> gemini-1.5-pro-latest
func extractModelFromPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "models" && i+1 < len(parts) {
			modelAndAction := strings.Split(parts[i+1], ":")
			return modelAndAction[0]
		}
	}
	return ""
}
