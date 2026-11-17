// Package config reads the file service settings from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults used when the matching environment variable is unset or unusable.
const (
	DefaultPort       = "8080"
	DefaultS3Region   = "us-east-1"
	DefaultPresignTTL = 15 * time.Minute
	maxPresignTTL     = 7 * 24 * time.Hour
	defaultForcePath  = true
)

// S3 is the S3-compatible object-store configuration.
type S3 struct {
	Endpoint       string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	ForcePathStyle bool
	PresignTTL     time.Duration
}

// Configured reports whether every value needed to build an S3 client is set.
func (s S3) Configured() bool {
	return s.Endpoint != "" && s.Bucket != "" && s.AccessKey != "" && s.SecretKey != ""
}

// Config is the service configuration.
type Config struct {
	Port string
	S3   S3
}

// Load reads the configuration through the process environment.
func Load() Config {
	return load(os.Getenv)
}

func load(getenv func(string) string) Config {
	port := strings.TrimSpace(getenv("PORT"))
	if port == "" {
		port = DefaultPort
	}

	region := strings.TrimSpace(getenv("S3_REGION"))
	if region == "" {
		region = DefaultS3Region
	}

	ttl := DefaultPresignTTL
	if raw := strings.TrimSpace(getenv("PRESIGN_TTL_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			ttl = time.Duration(seconds) * time.Second
			if ttl > maxPresignTTL {
				ttl = maxPresignTTL
			}
		}
	}

	return Config{
		Port: port,
		S3: S3{
			Endpoint:       strings.TrimSpace(getenv("S3_ENDPOINT")),
			Region:         region,
			Bucket:         strings.TrimSpace(getenv("S3_BUCKET")),
			AccessKey:      strings.TrimSpace(getenv("S3_ACCESS_KEY")),
			SecretKey:      strings.TrimSpace(getenv("S3_SECRET_KEY")),
			ForcePathStyle: boolFromEnv(getenv, "S3_FORCE_PATH_STYLE", defaultForcePath),
			PresignTTL:     ttl,
		},
	}
}

func boolFromEnv(getenv func(string) string, name string, fallback bool) bool {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}
