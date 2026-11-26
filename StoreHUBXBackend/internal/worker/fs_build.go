package worker

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuwanshb005/storehubx/internal/models"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func unzip(srcZip, destDir string) (string, error) {
	r, err := zip.OpenReader(srcZip)
	if err != nil {
		return "", err
	}
	defer r.Close()

	var topDir string

	for _, f := range r.File {
		fpath := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return "", fmt.Errorf("zip entry escapes extraction dir: %s", f.Name)
		}
		// remember the zipball top-level dir (GitHub names it owner-repo-sha/)
		if topDir == "" {
			parts := strings.Split(f.Name, "/")
			if len(parts) > 0 {
				topDir = filepath.Join(destDir, parts[0])
			}
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		out, err := os.Create(fpath)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return "", err
		}
	}
	return topDir, nil
}

func (p *Processor) maybeBuildWithNode(ctx context.Context, jobID primitive.ObjectID, workingDir string) error {
	if _, err := os.Stat(filepath.Join(workingDir, "package.json")); err == nil {
		logf := func(line string) { p.logPush(ctx, jobID, line) }
		if err := runNodeBuild(ctx, "storehubx-build-"+jobID.Hex(), workingDir, logf); err != nil {
			return err
		}
		for _, out := range []string{"dist", "build"} {
			if _, err := os.Stat(filepath.Join(workingDir, out)); err == nil {
				return nil
			}
		}
	}
	// fallback: if there is an index.html, we can ship that folder as-is
	if _, err := os.Stat(filepath.Join(workingDir, "index.html")); err == nil {
		return nil
	}
	return fmt.Errorf("%w: no build output found (need package.json+build or index.html)", errBuildFailed)
}

func pickOutputDir(workingDir string) (string, error) {
	for _, cand := range []string{"dist", "build", "."} {
		p := filepath.Join(workingDir, cand)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no output directory")
}

// detectContentType returns MIME type for a file based on extension
func detectContentType(pathOrName string) string {
	ext := strings.ToLower(filepath.Ext(pathOrName))
	switch ext {
	case ".js":
		return "application/javascript"
	case ".mjs":
		return "application/javascript"
	case ".css":
		return "text/css"
	case ".html", ".htm":
		return "text/html"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".eot":
		return "application/vnd.ms-fontobject"
	case ".otf":
		return "font/otf"
	case ".map":
		return "application/json"
	case ".txt":
		return "text/plain"
	case ".xml":
		return "application/xml"
	default:
		return "application/octet-stream"
	}
}

// modifyIndexHTMLOnDisk injects StoreHUBX metadata (component name/version/id,
// build timestamp) into index.html's <head>, in place on disk, before the S3
// uploader reads it.
func modifyIndexHTMLOnDisk(ctx context.Context, jobID primitive.ObjectID, indexPath string, job *models.BuildJob, logFunc func(context.Context, primitive.ObjectID, string)) error {
	content, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("index.html not found: %w", err)
	}
	modified := string(content)

	metaTags := fmt.Sprintf(`
    <!-- StoreHUBX Component Metadata -->
    <meta name="component-name" content="%s">
    <meta name="component-version" content="%s">
    <meta name="build-timestamp" content="%s">
    <meta name="component-id" content="%s">`,
		job.Component,
		job.Version,
		job.CreatedAt.Format("2006-01-02T15:04:05Z"),
		job.ComponentID.Hex(),
	)
	configScript := fmt.Sprintf(`
    <script>
        window.__STOREHUBX_COMPONENT__ = {
            name: "%s",
            version: "%s",
            componentId: "%s",
            buildTimestamp: "%s"
        };
    </script>`,
		job.Component,
		job.Version,
		job.ComponentID.Hex(),
		job.CreatedAt.Format("2006-01-02T15:04:05Z"),
	)

	if i := strings.Index(modified, "</head>"); i != -1 {
		modified = modified[:i] + metaTags + "\n" + configScript + "\n" + modified[i:]
	}

	if err := os.WriteFile(indexPath, []byte(modified), 0644); err != nil {
		return fmt.Errorf("failed to write modified index.html: %w", err)
	}

	logFunc(ctx, jobID, "injected component metadata into index.html")
	return nil
}
