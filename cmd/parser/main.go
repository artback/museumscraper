package main

import (
	"context"
	"fmt"
	"github.com/joho/godotenv"
	"log"
	"museum/internal/keys"
	"museum/internal/storage"
	"museum/pkg/wikipedia"
	"os"
	"strings"
	"time"
)

// sanitizeKey replaces non-alphanumeric characters with hyphens to create a valid object key.
func sanitizeKey(s string) string {
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ToLower(s)
	return s
}
func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
	bucketName := os.Getenv("MUSEUM_BUCKET_NAME")
	ctx := context.Background()
	start := time.Now() // record start time
	fmt.Println("Starting to parse 'Category:Lists_of_museums_by_country' and its subcategories...")
	client := wikipedia.NewClient()
	service := wikipedia.NewCategoryService(client)
	extractor := wikipedia.NewMuseumExtractor([]string{"Tourism", "Culture", "History", "UNESCO"})

	processor := wikipedia.NewCategoryProcessor(service, extractor)

	s3Service, err := storage.NewS3Service(keys.Museum)
	if err != nil {
		log.Fatal(err)
	}
	_, err = s3Service.CreateBucket(ctx, bucketName, "")
	if err != nil {
		log.Fatal(err)
	}

	museumCh := processor.ProcessCategoryAsync("Category:Lists_of_museums_by_country")
	s3Service.StoreFromChannel(ctx, bucketName, museumCh)

	elapsed := time.Since(start)
	fmt.Printf("\nFinished parsing all pages, took %s\n", elapsed)
}
