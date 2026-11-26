package db

import (
	"context"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var Client *mongo.Client

// Init initializes Mongo connection
func Init(uri string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		log.Fatal("Mongo connect error:", err)
	}

	if err = client.Ping(ctx, nil); err != nil {
		log.Fatal("Mongo ping error:", err)
	}

	Client = client
	log.Println("✅ Connected to MongoDB")
}

// Disconnect closes the Mongo connection
func Disconnect() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Client.Disconnect(ctx); err != nil {
		log.Println("Mongo disconnect error:", err)
	}
	log.Println("🛑 MongoDB connection closed")
}

// Name is the MongoDB database every process uses, from MONGO_DB (default
// "storehub"). The API and the worker both resolve it here so they can never
// disagree, and tests point it at a throwaway database.
func Name() string {
	if n := os.Getenv("MONGO_DB"); n != "" {
		return n
	}
	return "storehub"
}

// DB returns the application database handle.
func DB() *mongo.Database {
	return Client.Database(Name())
}
