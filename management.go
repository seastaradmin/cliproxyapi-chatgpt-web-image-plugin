package main

import (
	"embed"
	"encoding/json"
	"net/http"

	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginapi"
)

//go:embed ui/index.html ui/app.js ui/style.css
var managementFiles embed.FS

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{Resources: []pluginapi.ResourceRoute{
		{Path: "/index", Menu: "ChatGPT 网页生图", Description: "文生图与多图参考"},
		{Path: "/app.js"},
		{Path: "/style.css"},
	}}
}

// Resources are public and serve only static files. Generation uses CPA's authenticated image API.
func managementResource(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if json.Unmarshal(raw, &req) != nil {
		return nil, fail(400, "invalid_request_error", "Invalid resource request")
	}
	if req.Method != http.MethodGet {
		return nil, fail(405, "method_not_allowed", "Only GET resources are supported")
	}
	var file, contentType string
	switch req.Path {
	case "/v0/resource/plugins/" + pluginID + "/index":
		file, contentType = "ui/index.html", "text/html; charset=utf-8"
	case "/v0/resource/plugins/" + pluginID + "/app.js":
		file, contentType = "ui/app.js", "text/javascript; charset=utf-8"
	case "/v0/resource/plugins/" + pluginID + "/style.css":
		file, contentType = "ui/style.css", "text/css; charset=utf-8"
	default:
		return nil, fail(404, "not_found", "Resource not found")
	}
	body, errRead := managementFiles.ReadFile(file)
	if errRead != nil {
		return nil, fail(500, "resource_error", "Cannot read plugin resource")
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{
		"Content-Type": {contentType}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"},
		"Referrer-Policy":         {"no-referrer"},
		"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; img-src data: blob:; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"},
	}})
}
