package router

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/logger"
)

// serveFrontendStatic registers a middleware that serves the frontend SPA.
// It prefers WEKNORA_WEB_DIR when set, otherwise it falls back to the built-in
// ./web directory and then the repo checkout's ./frontend directory.
func resolveFrontendWebDir() string {
	if webDir := strings.TrimSpace(os.Getenv("WEKNORA_WEB_DIR")); webDir != "" {
		return webDir
	}
	for _, candidate := range []string{"./web", "./frontend"} {
		if _, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil {
			return candidate
		}
	}
	return "./web"
}

func serveFrontendStatic(r *gin.Engine) {
	webDir := resolveFrontendWebDir()
	absDir, _ := filepath.Abs(webDir)
	indexPath := filepath.Join(absDir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		return
	}

	logger.Infof(context.Background(), "[Router] Serving frontend static files from %s", absDir)

	fs := http.Dir(absDir)
	fileServer := http.FileServer(fs)

	r.Use(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/health") || strings.HasPrefix(path, "/swagger/") ||
			strings.HasPrefix(path, "/r/") || path == "/files" {
			c.Next()
			return
		}
		// Embed pages need the dedicated entry point and the channel CSP set by
		// embedFrameAncestorsMiddleware. Keep the main SPA same-origin only.
		if strings.HasPrefix(path, "/embed/") {
			c.File(filepath.Join(absDir, "embed.html"))
			c.Abort()
			return
		}
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Header("Content-Security-Policy", "frame-ancestors 'self'")
		fullPath := filepath.Join(absDir, path)
		if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
			setFrontendCacheHeaders(c.Writer, path)
			fileServer.ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}
		setFrontendCacheHeaders(c.Writer, "/index.html")
		c.File(indexPath)
		c.Abort()
	})
}

// setFrontendCacheHeaders sets Cache-Control headers for frontend static resources.
// Vite 构建产物中 /assets/* 的文件名带 hash，可长期缓存；其余（index.html、config.js、favicon 等）
// 每次都需 revalidate，避免前端升级后用户看到旧版本。
func setFrontendCacheHeaders(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
}
