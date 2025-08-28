package streaming

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gemini-antiblock/config"
	"gemini-antiblock/logger"
)

var nonRetryableStatuses = map[int]bool{
	400: true, 401: true, 403: true, 404: true, 429: true,
}

// endsWithSentencePunctuation returns true if the given text ends with a sentence-ending punctuation.
// The set includes common Chinese and English sentence terminators and closing quotes.
func endsWithSentencePunctuation(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 {
		return false
	}

	// Check for newline at the end (before trimming)
	if strings.HasSuffix(text, "\n") {
		return true
	}

	runes := []rune(trimmed)
	last := runes[len(runes)-1]

	// Complete punctuation set: original + requested additions
	// Using raw string to avoid quote escaping issues
	const punctuations = `.!?…'"》>。？！}])()`
	return strings.ContainsRune(punctuations, last)
}

// isValidResponseStructure checks if an SSE data line has valid response structure
func isValidResponseStructure(line string) bool {
	if !strings.HasPrefix(line, "data: ") {
		return true // Non-data lines are considered valid
	}

	// Extract JSON content from data line
	idx := strings.Index(line, "{")
	if idx == -1 {
		return false // No JSON content found
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(line[idx:]), &data); err != nil {
		logger.LogDebug("JSON parsing failed during structure validation:", err)
		return false
	}

	// Check if candidates field exists and is a valid array
	candidates, exists := data["candidates"]
	if !exists {
		logger.LogDebug("Missing candidates field in response structure")
		return false
	}

	candidatesArray, isArray := candidates.([]interface{})
	if !isArray {
		logger.LogDebug("Candidates field is not an array")
		return false
	}

	if len(candidatesArray) == 0 {
		logger.LogDebug("Candidates array is empty")
		return false
	}

	// Check if first candidate has valid structure
	if _, ok := candidatesArray[0].(map[string]interface{}); ok {
		// Basic structure validation - candidate should be a map
		logger.LogDebug("Response structure validation passed")
		return true
	}

	logger.LogDebug("First candidate has invalid structure")
	return false
}

// BuildRetryRequestBody builds a new request body for retry with accumulated context
func BuildRetryRequestBody(originalBody map[string]interface{}, accumulatedText string) (map[string]interface{}, error) {
	logger.LogDebug(fmt.Sprintf("Building retry request body. Accumulated text length: %d", len(accumulatedText)))
	logger.LogDebug(fmt.Sprintf("Accumulated text preview: %s", func() string {
		if len(accumulatedText) > 200 {
			return accumulatedText[:200] + "..."
		}
		return accumulatedText
	}()))

	retryBody := make(map[string]interface{})
	for k, v := range originalBody {
		retryBody[k] = v
	}

	contents, ok := retryBody["contents"].([]interface{})
	if !ok {
		logger.LogError("Failed to extract contents from retry body - contents field missing or invalid type")
		return nil, fmt.Errorf("invalid contents field in retry request")
	}

	if len(contents) == 0 {
		logger.LogError("Retry body contains empty contents array")
		return nil, fmt.Errorf("retry request cannot have empty contents")
	}

	// Find last user message index
	lastUserIndex := -1
	for i := len(contents) - 1; i >= 0; i-- {
		if content, ok := contents[i].(map[string]interface{}); ok {
			if role, ok := content["role"].(string); ok && role == "user" {
				lastUserIndex = i
				break
			}
		}
	}

	// Build retry context
	history := []interface{}{
		map[string]interface{}{
			"role": "model",
			"parts": []interface{}{
				map[string]interface{}{"text": accumulatedText},
			},
		},
		map[string]interface{}{
			"role": "user",
			"parts": []interface{}{
				map[string]interface{}{"text": "Continue exactly where you left off without any preamble or repetition."},
			},
		},
	}

	// Insert history after last user message
	if lastUserIndex != -1 {
		newContents := make([]interface{}, 0, len(contents)+2)
		newContents = append(newContents, contents[:lastUserIndex+1]...)
		newContents = append(newContents, history...)
		newContents = append(newContents, contents[lastUserIndex+1:]...)
		retryBody["contents"] = newContents
		logger.LogDebug(fmt.Sprintf("Inserted retry context after user message at index %d", lastUserIndex))
	} else {
		newContents := append(contents, history...)
		retryBody["contents"] = newContents
		logger.LogDebug("Appended retry context to end of conversation")
	}

	finalContents := retryBody["contents"].([]interface{})
	if len(finalContents) == 0 {
		logger.LogError("CRITICAL: Final retry request body has empty contents array")
		return nil, fmt.Errorf("final retry body validation failed: empty contents")
	}

	logger.LogDebug(fmt.Sprintf("Final retry request has %d messages", len(finalContents)))
	// 记录第一条和最后一条消息的基本信息用于调试
	if len(finalContents) > 0 {
		if firstMsg, ok := finalContents[0].(map[string]interface{}); ok {
			logger.LogDebug(fmt.Sprintf("First message role: %v", firstMsg["role"]))
		}
		if len(finalContents) > 1 {
			if lastMsg, ok := finalContents[len(finalContents)-1].(map[string]interface{}); ok {
				logger.LogDebug(fmt.Sprintf("Last message role: %v", lastMsg["role"]))
			}
		}
	}

	// Final validation and logging before returning
	if _, ok := retryBody["contents"]; !ok {
		logger.LogError("CRITICAL: 'contents' field is missing from the final retry body")
		return nil, fmt.Errorf("final retry body validation failed: 'contents' field missing")
	}
	if logger.IsDebugMode() {
		debugRetryBodyBytes, _ := json.Marshal(retryBody)
		logger.LogDebug("Final retry request body:", string(debugRetryBodyBytes))
	}

	return retryBody, nil
}

// ProcessStreamAndRetryInternally handles streaming with internal retry logic
func ProcessStreamAndRetryInternally(cfg *config.Config, initialReader io.Reader, writer io.Writer, originalRequestBody map[string]interface{}, upstreamURL string, originalHeaders http.Header) error {
	var accumulatedText string
	consecutiveRetryCount := 0
	currentReader := initialReader
	totalLinesProcessed := 0
	sessionStartTime := time.Now()

	isOutputtingFormalText := false
	swallowModeActive := false
	// Counts consecutive resume attempts (after at least one retry) whose last formal text ends with sentence punctuation
	resumePunctStreak := 0

	// Get maxOutputTokens from client request, with a default fallback
	maxOutputChars := 65535 // Default value
	if genConfig, ok := originalRequestBody["generationConfig"].(map[string]interface{}); ok {
		if maxTokens, ok := genConfig["maxOutputTokens"].(float64); ok && maxTokens > 0 {
			maxOutputChars = int(maxTokens)
			logger.LogInfo(fmt.Sprintf("Client-specified maxOutputTokens found, character limit set to: %d", maxOutputChars))
		}
	}

	logger.LogInfo(fmt.Sprintf("Starting stream processing session. Max retries: %d", cfg.MaxConsecutiveRetries))

	for {
		interruptionReason := ""
		cleanExit := false
		streamStartTime := time.Now()
		linesInThisStream := 0
		textInThisStream := ""

		logger.LogDebug(fmt.Sprintf("=== Starting stream attempt %d/%d ===", consecutiveRetryCount+1, cfg.MaxConsecutiveRetries+1))

		// Create channel for SSE lines
		lineCh := make(chan string, 100)
		go SSELineIterator(currentReader, lineCh)

		// Track the last formal text chunk seen in this attempt
		attemptLastFormalText := ""
		attemptLastFormalDataLine := ""
		attemptLastFormalTextFlushed := false

		// Process lines
		for line := range lineCh {
			totalLinesProcessed++
			linesInThisStream++

			var textChunk string
			var isThought bool

			if IsDataLine(line) {
				content := ParseLineContent(line)
				textChunk = content.Text
				isThought = content.IsThought
			}

			// Thought swallowing logic
			if swallowModeActive {
				if isThought {
					logger.LogDebug("Swallowing thought chunk due to post-retry filter:", line)
					finishReason := ExtractFinishReason(line)
					if finishReason != "" {
						logger.LogError(fmt.Sprintf("Stream stopped with reason '%s' while swallowing a 'thought' chunk. Triggering retry.", finishReason))
						interruptionReason = "FINISH_DURING_THOUGHT"
						break
					}
					continue
				} else {
					logger.LogInfo("First formal text chunk received after swallowing. Resuming normal stream.")
					swallowModeActive = false
				}
			}

			// Record the last formal text chunk for this attempt as early as possible,
			// so even if this line triggers a retry (e.g., STOP but considered incomplete),
			// it is still considered in cross-attempt punctuation heuristic.
			if textChunk != "" && !isThought {
				attemptLastFormalText = textChunk
				attemptLastFormalDataLine = line
				attemptLastFormalTextFlushed = false
			}

			// Retry decision logic
			finishReason := ExtractFinishReason(line)
			needsRetry := false

			if finishReason != "" && isThought {
				logger.LogError(fmt.Sprintf("Stream stopped with reason '%s' on a 'thought' chunk. This is an invalid state. Triggering retry.", finishReason))
				interruptionReason = "FINISH_DURING_THOUGHT"
				needsRetry = true
			} else if IsBlockedLine(line) {
				logger.LogError(fmt.Sprintf("Content blocked detected in line: %s", line))
				interruptionReason = "BLOCK"
				needsRetry = true
			} else if finishReason == "STOP" {
				tempAccumulatedText := accumulatedText + textChunk
				trimmedText := strings.TrimSpace(tempAccumulatedText)

				// Check for empty response - if we have STOP but no accumulated text at all, it's incomplete
				if len(trimmedText) == 0 {
					logger.LogError("Finish reason 'STOP' with no text content detected. This indicates an empty response. Triggering retry.")
					interruptionReason = "FINISH_EMPTY_RESPONSE"
					needsRetry = true
				} else if !strings.HasSuffix(trimmedText, "[done]") {
					runes := []rune(trimmedText)
					lastChar := string(runes[len(runes)-1])
					logger.LogError(fmt.Sprintf("Finish reason 'STOP' treated as incomplete because text ends with '%s'. Triggering retry.", lastChar))
					interruptionReason = "FINISH_INCOMPLETE"
					needsRetry = true
				}
			} else if finishReason != "" && finishReason != "MAX_TOKENS" && finishReason != "STOP" {
				logger.LogError(fmt.Sprintf("Abnormal finish reason: %s. Triggering retry.", finishReason))
				interruptionReason = "FINISH_ABNORMAL"
				needsRetry = true
			}

			if needsRetry {
				break
			}

			// Line is good: forward and update state
			isEndOfResponse := finishReason == "STOP" || finishReason == "MAX_TOKENS"
			processedLine := RemoveDoneTokenFromLine(line, isEndOfResponse)

			if _, err := writer.Write([]byte(processedLine + "\n\n")); err != nil {
				return fmt.Errorf("failed to write to output stream: %w", err)
			}

			// Flush the response to ensure data is sent immediately to the client
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}

			if textChunk != "" && !isThought {
				isOutputtingFormalText = true
				accumulatedText += textChunk
				textInThisStream += textChunk
				attemptLastFormalTextFlushed = true
			}

			// Check for total output character limit
			if maxOutputChars > 0 && len(accumulatedText) >= maxOutputChars {
				logger.LogInfo(fmt.Sprintf("Total output character limit (%d) reached. Treating as a clean exit.", maxOutputChars))
				cleanExit = true
				break
			}

			if finishReason == "STOP" || finishReason == "MAX_TOKENS" {
				logger.LogInfo(fmt.Sprintf("Finish reason '%s' accepted as final. Stream complete.", finishReason))
				cleanExit = true
				break
			}
		}

		if !cleanExit && interruptionReason == "" {
			logger.LogError("Stream ended without finish reason - detected as DROP")
			interruptionReason = "DROP"
		}

		streamDuration := time.Since(streamStartTime)
		logger.LogDebug("Stream attempt summary:")
		logger.LogDebug(fmt.Sprintf("  Duration: %v", streamDuration))
		logger.LogDebug(fmt.Sprintf("  Lines processed: %d", linesInThisStream))
		logger.LogDebug(fmt.Sprintf("  Text generated this stream: %d chars", len(textInThisStream)))
		logger.LogDebug(fmt.Sprintf("  Total accumulated text: %d chars", len(accumulatedText)))

		// Cross-attempt heuristic (optional): if we are in a resumed attempt (after at least one retry)
		// and the last formal text of this attempt ends with sentence punctuation, count streak.
		// If we reach 3 such consecutive resume attempts, treat as success and finish.
		if cfg.EnablePunctuationHeuristic && !cleanExit && consecutiveRetryCount > 0 {
			if attemptLastFormalText != "" && endsWithSentencePunctuation(attemptLastFormalText) {
				resumePunctStreak++
				logger.LogInfo(fmt.Sprintf("Resume punctuation streak incremented to %d (last formal text ends with sentence punctuation)", resumePunctStreak))
			} else {
				if attemptLastFormalText == "" {
					logger.LogDebug("No formal text in this attempt; resetting resume punctuation streak to 0")
				} else {
					logger.LogDebug("Last formal text does not end with sentence punctuation; resetting resume punctuation streak to 0")
				}
				resumePunctStreak = 0
			}

			if resumePunctStreak >= 3 {
				logger.LogInfo("Treating stream as successful due to 3 consecutive resume attempts ending with sentence punctuation.")
				// If the last formal text of this attempt was not flushed due to early interruption,
				// flush it now so the client receives the most recent block.
				if !attemptLastFormalTextFlushed && attemptLastFormalDataLine != "" {
					isEnd := ExtractFinishReason(attemptLastFormalDataLine)
					shouldRemove := isEnd == "STOP" || isEnd == "MAX_TOKENS"
					processed := RemoveDoneTokenFromLine(attemptLastFormalDataLine, shouldRemove)
					if _, err := writer.Write([]byte(processed + "\n\n")); err == nil {
						if flusher, ok := writer.(http.Flusher); ok {
							flusher.Flush()
						}
						// Keep accounting consistent
						accumulatedText += attemptLastFormalText
						textInThisStream += attemptLastFormalText
						isOutputtingFormalText = true
					}
				}
				cleanExit = true
			}
		}

		if cleanExit {
			sessionDuration := time.Since(sessionStartTime)
			logger.LogInfo("=== STREAM COMPLETED SUCCESSFULLY ===")
			logger.LogInfo(fmt.Sprintf("Total session duration: %v", sessionDuration))
			logger.LogInfo(fmt.Sprintf("Total lines processed: %d", totalLinesProcessed))
			logger.LogInfo(fmt.Sprintf("Total text generated: %d characters", len(accumulatedText)))
			logger.LogInfo(fmt.Sprintf("Total retries needed: %d", consecutiveRetryCount))
			return nil
		}

		// Interruption & Retry Activation
		logger.LogError("=== STREAM INTERRUPTED ===")
		logger.LogError(fmt.Sprintf("Reason: %s", interruptionReason))

		if cfg.SwallowThoughtsAfterRetry && isOutputtingFormalText {
			logger.LogInfo("Retry triggered after formal text output. Will swallow subsequent thought chunks until formal text resumes.")
			swallowModeActive = true
		}

		logger.LogError(fmt.Sprintf("Current retry count: %d", consecutiveRetryCount))
		logger.LogError(fmt.Sprintf("Max retries allowed: %d", cfg.MaxConsecutiveRetries))
		logger.LogError(fmt.Sprintf("Text accumulated so far: %d characters", len(accumulatedText)))

		if consecutiveRetryCount >= cfg.MaxConsecutiveRetries {
			errorPayload := map[string]interface{}{
				"error": map[string]interface{}{
					"code":    504,
					"status":  "DEADLINE_EXCEEDED",
					"message": fmt.Sprintf("Retry limit (%d) exceeded after stream interruption. Last reason: %s.", cfg.MaxConsecutiveRetries, interruptionReason),
					"details": []interface{}{
						map[string]interface{}{
							"@type":                  "proxy.debug",
							"accumulated_text_chars": len(accumulatedText),
						},
					},
				},
			}

			errorBytes, _ := json.Marshal(errorPayload)
			writer.Write([]byte(fmt.Sprintf("event: error\ndata: %s\n\n", string(errorBytes))))

			// Flush the error response to ensure it's sent immediately
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}

			return fmt.Errorf("retry limit exceeded")
		}

		consecutiveRetryCount++
		logger.LogInfo(fmt.Sprintf("=== STARTING RETRY %d/%d ===", consecutiveRetryCount, cfg.MaxConsecutiveRetries))

		// Build retry request
		retryBody, err := BuildRetryRequestBody(originalRequestBody, accumulatedText)
		if err != nil {
			logger.LogError("Failed to build retry request body:", err)
			// 发送错误到客户端而不是继续重试
			errorPayload := map[string]interface{}{
				"error": map[string]interface{}{
					"code":    400,
					"status":  "INVALID_ARGUMENT",
					"message": "Failed to build retry request: " + err.Error(),
				},
			}
			errorBytes, _ := json.Marshal(errorPayload)
			writer.Write([]byte(fmt.Sprintf("event: error\ndata: %s\n\n", string(errorBytes))))
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
			return fmt.Errorf("retry request validation failed: %w", err)
		}

		// Log the retry request body for debugging
		prettyBodyBytes, _ := json.MarshalIndent(retryBody, "  ", "  ")
		f, err := os.OpenFile("debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			f.WriteString("\n--- RETRY REQUEST ---")
			f.Write(prettyBodyBytes)
			f.Close()
		}

		retryBodyBytes, err := json.Marshal(retryBody)
		if err != nil {
			logger.LogError("Failed to marshal retry body:", err)
			time.Sleep(cfg.RetryDelayMs)
			continue
		}

		// Create retry request
		retryReq, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(retryBodyBytes))
		if err != nil {
			logger.LogError("Failed to create retry request:", err)
			time.Sleep(cfg.RetryDelayMs)
			continue
		}

		// Copy headers
		for name, values := range originalHeaders {
			if name == "Authorization" || name == "X-Goog-Api-Key" || name == "Content-Type" || name == "Accept" {
				for _, value := range values {
					retryReq.Header.Add(name, value)
				}
			}
		}

		logger.LogDebug(fmt.Sprintf("Making retry request to: %s", upstreamURL))
		logger.LogDebug(fmt.Sprintf("Retry request body size: %d bytes", len(retryBodyBytes)))

		// Make retry request
		client := &http.Client{}
		retryResponse, err := client.Do(retryReq)
		if err != nil {
			logger.LogError(fmt.Sprintf("=== RETRY ATTEMPT %d FAILED ===", consecutiveRetryCount))
			logger.LogError("Exception during retry:", err)
			logger.LogError(fmt.Sprintf("Will wait %v before next attempt (if any)", cfg.RetryDelayMs))
			time.Sleep(cfg.RetryDelayMs)
			continue
		}

		logger.LogInfo(fmt.Sprintf("Retry request completed. Status: %d %s", retryResponse.StatusCode, retryResponse.Status))

		if nonRetryableStatuses[retryResponse.StatusCode] {
			logger.LogError("=== FATAL ERROR DURING RETRY ===")
			logger.LogError(fmt.Sprintf("Received non-retryable status %d during retry attempt %d", retryResponse.StatusCode, consecutiveRetryCount))

			// Write SSE error from upstream
			errorBytes, _ := io.ReadAll(retryResponse.Body)
			retryResponse.Body.Close()

			writer.Write([]byte(fmt.Sprintf("event: error\ndata: %s\n\n", string(errorBytes))))

			// Flush the error response to ensure it's sent immediately
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}

			return fmt.Errorf("non-retryable error: %d", retryResponse.StatusCode)
		}

		if retryResponse.StatusCode != http.StatusOK {
			logger.LogError(fmt.Sprintf("Retry attempt %d failed with status %d", consecutiveRetryCount, retryResponse.StatusCode))
			logger.LogError("This is considered a retryable error - will try again if retries remain")
			retryResponse.Body.Close()
			time.Sleep(cfg.RetryDelayMs)
			continue
		}

		logger.LogInfo(fmt.Sprintf("✓ Retry attempt %d successful - got new stream", consecutiveRetryCount))
		logger.LogInfo(fmt.Sprintf("Continuing with accumulated context (%d chars)", len(accumulatedText)))

		currentReader = retryResponse.Body
	}
}
