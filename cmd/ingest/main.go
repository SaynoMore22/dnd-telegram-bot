package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/flash4612/dnd-telegram-bot/internal/scrape"
	"github.com/flash4612/dnd-telegram-bot/internal/store"
)

func main() {
	_ = godotenv.Load()

	dbPath := flag.String("db", envOr("DATABASE_PATH", "data/dnd.db"), "sqlite path")
	baseURL := flag.String("base", envOr("DND_BASE_URL", "https://next.dnd.su"), "next.dnd.su base url")
	only := flag.String("only", "", "optional category: class|species|background|spell|item|equipment")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer st.Close()

	client := scrape.New(*baseURL)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	progress := func(msg string) { fmt.Println(msg) }

	if *only != "" {
		spec, ok := findCategory(*only)
		if !ok {
			log.Fatalf("unknown category %q", *only)
		}
		if err := client.IngestCategory(ctx, st, spec, progress); err != nil {
			log.Fatalf("ingest: %v", err)
		}
	} else {
		if err := client.IngestAll(ctx, st, progress); err != nil {
			log.Fatalf("ingest: %v", err)
		}
	}

	counts, err := st.CountByCategory(ctx)
	if err != nil {
		log.Fatalf("stats: %v", err)
	}
	total, _ := st.Count(ctx)
	fmt.Println("Готово:")
	for cat, n := range counts {
		fmt.Printf("  %s: %d\n", cat, n)
	}
	fmt.Printf("Всего: %d\n", total)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func findCategory(name string) (scrape.CategorySpec, bool) {
	for _, c := range scrape.Categories {
		if string(c.Category) == name {
			return c, true
		}
	}
	return scrape.CategorySpec{}, false
}
