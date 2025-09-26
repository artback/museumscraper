package main

import (
	"context"
	"fmt"
	"github.com/joho/godotenv"
	"log"
	"museum/internal/storage"
	wikipedia2 "museum/pkg/wikipedia"
	"os"
	"time"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
	bucketName := os.Getenv("MUSEUM_BUCKET_NAME")
	ctx := context.Background()
	start := time.Now() // record start time
	fmt.Println("Starting to parse 'Category:Lists_of_museums_by_country' and its subcategories...")
	client := wikipedia2.NewClient()
	service := wikipedia2.NewCategoryService(client)
	extractor := wikipedia2.NewMuseumExtractor([]string{"Tourism", "Culture", "History", "UNESCO"})

	processor := wikipedia2.NewCategoryProcessor(service, extractor)

	s3Service, err := storage.NewS3Service()
	if err != nil {
		log.Fatal(err)
	}
	_, err = s3Service.CreateBucket(ctx, bucketName, "")
	if err != nil {
		log.Fatal(err)
	}

	museumCh := processor.ProcessCategoryAsync("Category:Lists_of_museums_by_country")
	s3Service.StoreMuseumsFromChannel(ctx, bucketName, museumCh)

	elapsed := time.Since(start)
	fmt.Printf("\nFinished parsing all pages, took %s\n", elapsed)
}
