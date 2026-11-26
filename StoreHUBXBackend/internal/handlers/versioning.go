package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
)

// Automatic version numbers come from a counter stored on the component
// (components.autoVersion), advanced with a single atomic $inc. Two
// deploys arriving at the same moment therefore always get different
// numbers.
//
// The previous approach read "the latest version" and added one. Two
// concurrent deploys read the same latest version, picked the same number,
// and the unique index rejected all but one: those commits were silently
// never deployed (GitHub does not retry a failed webhook).

var (
	errDupCommit  = errors.New("a version already exists for this commit")
	errDupVersion = errors.New("version number already exists")
)

func parseSemver(s string) (models.SemVer, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return models.SemVer{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return models.SemVer{}, false
		}
		n[i] = v
	}
	return models.SemVer{Major: n[0], Minor: n[1], Patch: n[2]}, true
}

func semverLess(a, b models.SemVer) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	return a.Patch < b.Patch
}

// nextAutoVersion atomically reserves the next patch version.
func nextAutoVersion(ctx context.Context, componentID primitive.ObjectID) (string, error) {
	col := db.DB().Collection("components")
	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.After).
		SetProjection(bson.M{"autoVersion": 1})

	for attempt := 0; attempt < 3; attempt++ {
		var out struct {
			AutoVersion *models.SemVer `bson:"autoVersion"`
		}
		err := col.FindOneAndUpdate(ctx,
			bson.M{"_id": componentID, "autoVersion": bson.M{"$exists": true}},
			bson.M{"$inc": bson.M{"autoVersion.patch": 1}}, opts).Decode(&out)
		if err == nil && out.AutoVersion != nil {
			v := out.AutoVersion
			return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch), nil
		}
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return "", err
		}
		// First automatic version for this component: seed the counter
		// from the highest existing version. If several requests race
		// here, the $exists:false filter lets exactly one seed win.
		start, err := highestVersion(ctx, componentID)
		if err != nil {
			return "", err
		}
		if _, err := col.UpdateOne(ctx,
			bson.M{"_id": componentID, "autoVersion": bson.M{"$exists": false}},
			bson.M{"$set": bson.M{"autoVersion": start}}); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not reserve a version number")
}

// highestVersion is the greatest semver among existing versions, or 1.0.-1
// (so the first automatic version is 1.0.0).
func highestVersion(ctx context.Context, componentID primitive.ObjectID) (models.SemVer, error) {
	cur, err := db.DB().Collection("component_versions").Find(ctx,
		bson.M{"componentId": componentID}, options.Find().SetProjection(bson.M{"version": 1}))
	if err != nil {
		return models.SemVer{}, err
	}
	defer cur.Close(ctx)
	best := models.SemVer{Major: 1, Minor: 0, Patch: -1}
	for cur.Next(ctx) {
		var v struct {
			Version string `bson:"version"`
		}
		if cur.Decode(&v) == nil {
			if s, ok := parseSemver(v.Version); ok && semverLess(best, s) {
				best = s
			}
		}
	}
	return best, cur.Err()
}

// raiseAutoVersion moves the counter up to a manually chosen version, so
// the next automatic version follows it (2.3.0 -> 2.3.1). The filter makes
// it a no-op when the counter is already ahead, so it never moves back.
func raiseAutoVersion(ctx context.Context, componentID primitive.ObjectID, version string) {
	v, ok := parseSemver(version)
	if !ok {
		return
	}
	_, _ = db.DB().Collection("components").UpdateOne(ctx, bson.M{
		"_id": componentID,
		"$or": bson.A{
			bson.M{"autoVersion.major": bson.M{"$lt": v.Major}},
			bson.M{"autoVersion.major": v.Major, "autoVersion.minor": bson.M{"$lt": v.Minor}},
			bson.M{"autoVersion.major": v.Major, "autoVersion.minor": v.Minor, "autoVersion.patch": bson.M{"$lt": v.Patch}},
		},
	}, bson.M{"$set": bson.M{"autoVersion": v}})
}

// insertVersion inserts and classifies unique-index violations, which are
// the real arbiter of "already exists" under concurrency.
func insertVersion(ctx context.Context, v *models.ComponentVersion) error {
	res, err := db.DB().Collection("component_versions").InsertOne(ctx, v)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			if strings.Contains(err.Error(), "commitSha") {
				return errDupCommit
			}
			return errDupVersion
		}
		return err
	}
	v.ID = res.InsertedID.(primitive.ObjectID)
	return nil
}
