package main

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Address, UploadDir, AIKey, AIBaseURL, AIModel string
	AIDailyLimit, MatchCandidates                 int
}

func loadConfig() (Config, error) {
	c := Config{Address: envOr("APP_ADDR", ":8080"), UploadDir: envOr("APP_UPLOAD_DIR", "uploads"), AIKey: os.Getenv("DEEPSEEK_API_KEY"), AIBaseURL: envOr("DEEPSEEK_BASE_URL", "https://api.deepseek.com"), AIModel: envOr("DEEPSEEK_MODEL", "deepseek-flash"), AIDailyLimit: 100, MatchCandidates: 20}
	for _, s := range []struct {
		name   string
		target *int
		max    int
	}{{"AI_DAILY_CALL_LIMIT", &c.AIDailyLimit, 10000}, {"AI_MATCH_CANDIDATES", &c.MatchCandidates, 50}} {
		if raw := os.Getenv(s.name); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 || v > s.max {
				return c, fmt.Errorf("%s 必须是 1～%d 的整数", s.name, s.max)
			}
			*s.target = v
		}
	}
	return c, nil
}
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
