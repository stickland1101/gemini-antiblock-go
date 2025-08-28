package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration values
type Config struct {
	UpstreamURLBase            string
	MaxConsecutiveRetries      int
	DebugMode                  bool
	RetryDelayMs               time.Duration
	SwallowThoughtsAfterRetry  bool
	Port                       string
	EnableRateLimit            bool
	RateLimitCount             int
	RateLimitWindowSeconds     int
	EnablePunctuationHeuristic bool
	GeminiModelMaxTokens       map[string]int
	TokenLimitExceededCode     int
	TokenLimitExceededMessage  string
	NoRetryErrorCodes          []int

	// Anti-excessive continuation config
	PromptLengthThreshold int  // Skip [done] check if prompt > threshold
	DisableDoneTokenCheck bool // Global disable [done] token check
}

// LoadConfig loads configuration from environment variables
func LoadConfig() *Config {
	// Parse model max tokens JSON
	modelMaxTokens := make(map[string]int)
	if jsonStr := os.Getenv("GEMINI_MODEL_MAX_TOKENS_JSON"); jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &modelMaxTokens); err != nil {
			// Log error but continue with empty map
		}
	}

	// Parse no retry error codes
	var noRetryCodes []int
	if codesStr := os.Getenv("NO_RETRY_ERROR_CODES"); codesStr != "" {
		codes := strings.Split(codesStr, ",")
		for _, codeStr := range codes {
			if code, err := strconv.Atoi(strings.TrimSpace(codeStr)); err == nil {
				noRetryCodes = append(noRetryCodes, code)
			}
		}
	}

	return &Config{
		UpstreamURLBase:            getEnvString("UPSTREAM_URL_BASE", "https://generativelanguage.googleapis.com"),
		Port:                       getEnvString("PORT", "8080"),
		DebugMode:                  getEnvBool("DEBUG_MODE", true),
		MaxConsecutiveRetries:      getEnvInt("MAX_CONSECUTIVE_RETRIES", 100),
		RetryDelayMs:               time.Duration(getEnvInt("RETRY_DELAY_MS", 750)) * time.Millisecond,
		SwallowThoughtsAfterRetry:  getEnvBool("SWALLOW_THOUGHTS_AFTER_RETRY", true),
		EnableRateLimit:            getEnvBool("ENABLE_RATE_LIMIT", false),
		RateLimitCount:             getEnvInt("RATE_LIMIT_COUNT", 10),
		RateLimitWindowSeconds:     getEnvInt("RATE_LIMIT_WINDOW_SECONDS", 60),
		EnablePunctuationHeuristic: getEnvBool("ENABLE_PUNCTUATION_HEURISTIC", true),
		GeminiModelMaxTokens:       modelMaxTokens,
		TokenLimitExceededCode:     getEnvInt("TOKEN_LIMIT_EXCEEDED_CODE", 413),
		TokenLimitExceededMessage:  getEnvString("TOKEN_LIMIT_EXCEEDED_MESSAGE", "Request payload is too large: token count exceeds model limit."),
		NoRetryErrorCodes:          noRetryCodes,

		// Anti-excessive continuation config
		PromptLengthThreshold: getEnvInt("PROMPT_LENGTH_THRESHOLD", 10000),
		DisableDoneTokenCheck: getEnvBool("DISABLE_DONE_TOKEN_CHECK", false),
	}
}

func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}
