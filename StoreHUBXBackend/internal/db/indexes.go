package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// EnsureIndexes creates every index the app relies on. Several of them are
// correctness guarantees (unique version/commit per component, one like per
// user), not just speed-ups, so a failure is returned instead of ignored:
// silently running without them turns races into duplicate data.
func EnsureIndexes(client *mongo.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db := client.Database(Name())
	var err error
	var errs []error
	check := func(name string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	// components: slug unique
	_, err = db.Collection("components").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "slug", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	check("components", err)

	// components: weighted text index for search relevance (name > tags > description)
	_, err = db.Collection("components").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "name", Value: "text"},
			{Key: "tags", Value: "text"},
			{Key: "description", Value: "text"},
		},
		Options: options.Index().SetWeights(bson.D{
			{Key: "name", Value: 10},
			{Key: "tags", Value: 5},
			{Key: "description", Value: 1},
		}).SetName("components_text_search"),
	})
	check("components", err)

	// component_versions: componentId + version unique
	_, err = db.Collection("component_versions").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "componentId", Value: 1}, {Key: "version", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	check("component_versions", err)

	// component_versions: componentId + commitSha unique (prevent duplicate commits)
	_, err = db.Collection("component_versions").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "componentId", Value: 1}, {Key: "commitSha", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	check("component_versions", err)

	// interactions: one like/rating per user per component; comments allow
	// many per user, so the unique constraint only applies to like/rating.
	_, err = db.Collection("interactions").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "componentId", Value: 1}, {Key: "userId", Value: 1}, {Key: "type", Value: 1}},
		Options: options.Index().
			SetUnique(true).
			SetPartialFilterExpression(bson.M{"type": bson.M{"$in": bson.A{"like", "rating"}}}),
	})
	check("interactions", err)

	// interactions: paginated listing of comments/ratings for a component
	_, err = db.Collection("interactions").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "componentId", Value: 1}, {Key: "type", Value: 1}, {Key: "createdAt", Value: -1}},
		Options: nil,
	})
	check("interactions", err)

	// build_jobs: filter by component/version/status
	_, err = db.Collection("build_jobs").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "component", Value: 1}}},
		{Keys: bson.D{{Key: "version", Value: 1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}},
	})
	check("build_jobs", err)

	// collections: list a user's collections
	_, err = db.Collection("collections").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "ownerId", Value: 1}},
		Options: nil,
	})
	check("collections", err)

	// follows: a user can only follow the same target once
	_, err = db.Collection("follows").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "followerId", Value: 1}, {Key: "targetType", Value: 1}, {Key: "targetId", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	check("follows", err)

	// notifications: a user's feed, unread-first
	_, err = db.Collection("notifications").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "userId", Value: 1}, {Key: "read", Value: 1}, {Key: "createdAt", Value: -1}},
		Options: nil,
	})
	check("notifications", err)

	// webhook_deliveries: one row per processed GitHub delivery id (the
	// _id), expired after 7 days — GitHub only redelivers recent events.
	_, err = db.Collection("webhook_deliveries").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "receivedAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(7 * 24 * 3600),
	})
	check("webhook_deliveries", err)

	return errors.Join(errs...)
}
