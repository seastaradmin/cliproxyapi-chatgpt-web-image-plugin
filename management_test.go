package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginabi"
	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginapi"
)

func TestManagementMenuAndEmbeddedResources(t *testing.T) {
	old := hostCall
	defer func() { hostCall = old }()
	hostCall = func(string, any) (json.RawMessage, error) {
		t.Fatal("public resources must not access host credentials")
		return nil, nil
	}
	raw, err := handleMethod(pluginabi.MethodManagementRegister, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	if err = json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("registration envelope: %v", err)
	}
	var reg pluginapi.ManagementRegistrationResponse
	if err = json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Routes) != 0 || len(reg.Resources) != 3 || reg.Resources[0].Path != "/index" || reg.Resources[0].Menu != "ChatGPT 网页生图" {
		t.Fatalf("missing static menu registration: %+v", reg)
	}
	for _, resource := range reg.Resources {
		payload, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginID + resource.Path, Headers: http.Header{"Authorization": {"Bearer private-test-secret"}}})
		raw, err = handleMethod(pluginabi.MethodManagementHandle, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &env); err != nil || !env.OK {
			t.Fatalf("resource envelope: %v", err)
		}
		var response pluginapi.ManagementResponse
		if err = json.Unmarshal(env.Result, &response); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || len(response.Body) == 0 || strings.Contains(string(response.Body), "private-test-secret") {
			t.Fatal("static resource missing or leaks request credentials")
		}
		if response.Headers.Get("Cache-Control") != "no-store" || response.Headers.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(response.Headers.Get("Content-Security-Policy"), "connect-src 'self'") {
			t.Fatal("missing static resource security headers")
		}
		if resource.Path == "/index" && !strings.HasPrefix(response.Headers.Get("Content-Type"), "text/html") {
			t.Fatal("menu must serve HTML")
		}
	}
}

func TestPublicResourcesCannotGenerateOrReadArbitraryFiles(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/v0/resource/plugins/chatgpt-web-image/index", 405},
		{"GET", "/v0/resource/plugins/chatgpt-web-image/generate", 404},
		{"GET", "/v0/management/plugins/chatgpt-web-image/generate", 404},
		{"GET", "/v0/resource/plugins/chatgpt-web-image/../../config.yaml", 404},
		{"GET", "/v0/resource/plugins/other/index", 404},
	} {
		payload, _ := json.Marshal(pluginapi.ManagementRequest{Method: tc.method, Path: tc.path})
		_, err := managementResource(payload)
		var pe *pluginabi.Error
		if !errors.As(err, &pe) || pe.HTTPStatus != tc.status {
			t.Fatalf("%s %s: expected %d, got %v", tc.method, tc.path, tc.status, err)
		}
	}
}
