package web

import "embed"

//go:embed ui.html ui.js ui.css
var files embed.FS

// Asset exposes only the public, non-secret UI resources.
func Asset(name string) ([]byte, string, bool) {
	contentType := map[string]string{
		"ui.html": "text/html; charset=utf-8",
		"ui.js":   "text/javascript; charset=utf-8",
		"ui.css":  "text/css; charset=utf-8",
	}[name]
	if contentType == "" {
		return nil, "", false
	}
	body, err := files.ReadFile(name)
	return body, contentType, err == nil
}
