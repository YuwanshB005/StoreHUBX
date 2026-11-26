package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/yuwanshb005/storehubx/internal/cache"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/storage"
	"github.com/yuwanshb005/storehubx/internal/worker"
	"github.com/yuwanshb005/storehubx/internal/config"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	config.LoadConfig()
	db.Init(config.AppConfig.MongoURI)
	defer db.Disconnect()
	cache.Init()

	uploader, err := storage.NewS3Uploader()
	if err != nil {
		log.Fatal("s3 uploader:", err)
	}

	proc := worker.NewProcessor(uploader)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Println("storehub-worker: running")
	proc.Run(ctx)
	log.Println("storehub-worker: stopped")
}
