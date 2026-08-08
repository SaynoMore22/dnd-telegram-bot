package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	TelegramToken  string
	AllowedUserIDs map[int64]struct{}
	DatabasePath   string
	OpenAIKey      string
	BaseURL        string
}

func Load() (Config, error) {
	_ = godotenv.Load()

	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	dbPath := strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	if dbPath == "" {
		dbPath = "data/dnd.db"
	}

	baseURL := strings.TrimSpace(os.Getenv("DND_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://next.dnd.su"
	}

	ids, err := parseUserIDs(os.Getenv("ALLOWED_USER_IDS"))
	if err != nil {
		return Config{}, err
	}
	if len(ids) == 0 {
		return Config{}, fmt.Errorf("ALLOWED_USER_IDS must contain at least one Telegram user id")
	}

	return Config{
		TelegramToken:  token,
		AllowedUserIDs: ids,
		DatabasePath:   dbPath,
		OpenAIKey:      strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		BaseURL:        strings.TrimRight(baseURL, "/"),
	}, nil
}

func parseUserIDs(raw string) (map[int64]struct{}, error) {
	out := make(map[int64]struct{})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid ALLOWED_USER_IDS entry %q: %w", part, err)
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func (c Config) IsAllowed(userID int64) bool {
	_, ok := c.AllowedUserIDs[userID]
	return ok
}
