// @title StoreHUB API
// @version 1.0
// @description API documentation for StoreHUB backend.
// @host localhost:8080
// @BasePath /
// @schemes http

package main

import (
	"log"

	"github.com/joho/godotenv"
	"github.com/yuwanshb005/storehubx/internal/cache"
	"github.com/yuwanshb005/storehubx/internal/config"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/server"
)

func main() {
	_ = godotenv.Load()
	config.LoadConfig()
	db.Init(config.AppConfig.MongoURI)
	if err := db.EnsureIndexes(db.Client); err != nil {
		log.Fatal("ensure indexes: ", err)
	}
	defer db.Disconnect()
	cache.Init()

	app := server.New()
	log.Println("StoreHUB running on port", config.AppConfig.Port)
	log.Fatal(app.Listen(":" + config.AppConfig.Port))
}
