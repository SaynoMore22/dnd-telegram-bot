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

	"github.com/SaynoMore22/dnd-telegram-bot/internal/scrape"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/store"
)

func main() {
	_ = godotenv.Load()

	dbPath := flag.String("db", envOr("DATABASE_PATH", "data/dnd.db"), "sqlite path")
	edition := flag.String("edition", "2024", "редакция: 2024 (next.dnd.su) | 2014 (dnd.su)")
	baseURL := flag.String("base", "", "переопределить базовый URL (по умолчанию берётся по -edition)")
	only := flag.String("only", "", "optional category, e.g. class|species|background|spell|item|equipment|feat")
	flag.Parse()

	var cats []scrape.CategorySpec
	defaultBase := ""
	switch *edition {
	case "2024":
		cats = scrape.Categories
		defaultBase = "https://next.dnd.su"
	case "2014":
		cats = scrape.Categories2014
		defaultBase = "https://dnd.su"
	default:
		log.Fatalf("unknown edition %q (ожидается 2024 или 2014)", *edition)
	}
	if *baseURL == "" {
		// DND_BASE_URL исторически указывает на next.dnd.su — используем его только для
		// редакции 2024; для 2014 всегда берём дефолт по редакции.
		if *edition == "2024" {
			*baseURL = envOr("DND_BASE_URL", defaultBase)
		} else {
			*baseURL = defaultBase
		}
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer st.Close()

	client := scrape.NewEdition(*baseURL, *edition)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	progress := func(msg string) { fmt.Println(msg) }

	if *only != "" {
		spec, ok := findCategory(cats, *only)
		if !ok {
			log.Fatalf("unknown category %q for edition %s", *only, *edition)
		}
		if err := client.IngestCategory(ctx, st, spec, progress); err != nil {
			log.Fatalf("ingest: %v", err)
		}
	} else {
		if err := client.IngestCategories(ctx, st, cats, progress); err != nil {
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

func findCategory(cats []scrape.CategorySpec, name string) (scrape.CategorySpec, bool) {
	for _, c := range cats {
		if string(c.Category) == name {
			return c, true
		}
	}
	return scrape.CategorySpec{}, false
}
