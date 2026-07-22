package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Host               string
	Port               string
	DiskCache          bool
	DiskCacheLimit     int64 // MB
	MemoryCache        bool
	MemoryCacheLimit   int64 // MB
	FallbackDNS        string
	Preload            bool
	PreloadImages      bool
	ShowErrorsToClient bool
}

func Load() *Config {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, using environment variables")
	}

	return &Config{
		Host:               getEnv("FLINTERNET_HOST", "0.0.0.0"),
		Port:               getEnv("FLINTERNET_PORT", "8021"),
		DiskCache:          getEnv("FLINTERNET_DISK_CACHE", "false") == "true",
		DiskCacheLimit:     getEnvInt64("FLINTERNET_DISK_CACHE_LIMIT", 1024), // 1GB
		MemoryCache:        getEnv("FLINTERNET_MEMORY_CACHE", "false") == "true",
		MemoryCacheLimit:   getEnvInt64("FLINTERNET_MEMORY_CACHE_LIMIT", 50), // 50MB
		FallbackDNS:        getEnv("FLINTERNET_FALLBACK_DNS", ""),
		Preload:            getEnvBool("FLINTERNET_PRELOAD", false),
		PreloadImages:      getEnvBool("FLINTERNET_PRELOAD_IMAGES", false),
		ShowErrorsToClient: getEnvBool("FLINTERNET_SHOW_ERRORS_TO_CLIENT", false),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		boolValue, err := strconv.ParseBool(value)
		if err == nil {
			return boolValue
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if value, exists := os.LookupEnv(key); exists {
		intValue, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			return intValue
		}
	}
	return fallback
}
