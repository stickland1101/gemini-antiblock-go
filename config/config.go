package config

import (
	"os"
	"strconv"
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
}

// LoadConfig loads configuration from environment variables
func LoadConfig() *Config {
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
