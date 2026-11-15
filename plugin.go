package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginabi"
	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginID = "chatgpt-web-image"
const modelID = "gpt-image-2-web"

type pluginConfig struct {
	Enabled        bool   `yaml:"enabled"`
	RuntimeDir     string `yaml:"runtime_dir"`
	RouteGPTImage2 bool   `yaml:"route_gpt_image_2"`
	ProxyURL       string `yaml:"proxy_url"`
}

var currentConfig atomic.Value
var hostCall = callHost
var accounts = accountPool{busy: make(map[string]bool)}
var workerMu sync.Mutex
var workers = make(map[*exec.Cmd]struct{})
var stopping bool

type accountPool struct {
	mu   sync.Mutex
	busy map[string]bool
	next uint64
}

func (p *accountPool) reserve(files []pluginapi.HostAuthFileEntry, excluded map[string]bool) (pluginapi.HostAuthFileEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sort.Slice(files, func(i, j int) bool { return files[i].AuthIndex < files[j].AuthIndex })
	if len(files) == 0 {
		return pluginapi.HostAuthFileEntry{}, false
	}
	for offset := range files {
		index := (int(p.next%uint64(len(files))) + offset) % len(files)
		f := files[index]
		provider := f.Provider
		if provider == "" {
			provider = f.Type
		}
		if f.AuthIndex == "" || f.Disabled || f.Unavailable || excluded[f.AuthIndex] || p.busy[f.AuthIndex] || !strings.EqualFold(provider, "codex") {
			continue
		}
		p.busy[f.AuthIndex] = true
		p.next = uint64(index + 1)
		return f, true
	}
	return pluginapi.HostAuthFileEntry{}, false
}

func (p *accountPool) release(index string) { p.mu.Lock(); delete(p.busy, index); p.mu.Unlock() }

type credential struct {
	AccessToken string `json:"access_token"`
	PlanType    string `json:"plan_type"`
	IDToken     string `json:"id_token"`
	Type        string `json:"type"`
	Disabled    bool   `json:"disabled"`
	ProxyURL    string `json:"proxy_url"`
}

func decodeCredential(raw []byte, now time.Time) (credential, bool) {
	var c credential
	if json.Unmarshal(raw, &c) != nil || c.Disabled || c.Type != "codex" || c.AccessToken == "" {
		return c, false
	}
	if c.PlanType == "" {
		var claims struct {
			Auth struct {
				Plan string `json:"chatgpt_plan_type"`
			} `json:"https://api.openai.com/auth"`
		}
		_ = json.Unmarshal(jwtPayload(c.IDToken), &claims)
		c.PlanType = claims.Auth.Plan
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(jwtPayload(c.AccessToken), &claims) != nil || claims.Exp <= now.Unix() {
		return c, false
	}
	return c, strings.EqualFold(c.PlanType, "free")
}

func jwtPayload(token string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	data, _ := base64.RawURLEncoding.DecodeString(parts[1])
	return data
}

type imageRequest struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	N              *int     `json:"n,omitempty"`
	Size           string   `json:"size,omitempty"`
	Quality        string   `json:"quality,omitempty"`
	ResponseFormat string   `json:"response_format,omitempty"`
	Stream         bool     `json:"stream,omitempty"`
	Images         []string `json:"images,omitempty"`
}

func validateImageRequest(raw []byte) (imageRequest, error) {
	var req imageRequest
	if len(raw) > maxRequestBytes || json.Unmarshal(raw, &req) != nil {
		return req, fail(400, "invalid_request_error", "Invalid image request JSON")
	}
	if req.Model != modelID && req.Model != "gpt-image-2" {
		return req, fail(400, "model_not_found", "Unsupported web image model")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return req, fail(400, "invalid_request_error", "prompt is required")
	}
	if req.N != nil && *req.N != 1 {
		return req, fail(400, "invalid_request_error", "Web image generation currently supports n=1")
	}
	if req.Stream {
		return req, fail(400, "invalid_request_error", "Web image generation currently requires stream=false")
	}
	if req.ResponseFormat == "" {
		req.ResponseFormat = "b64_json"
	}
	if req.ResponseFormat != "b64_json" && req.ResponseFormat != "url" {
		return req, fail(400, "invalid_request_error", "response_format must be b64_json or url")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for key := range fields {
		switch key {
		case "model", "prompt", "n", "size", "quality", "response_format", "stream", "user", "images":
		default:
			return req, fail(400, "invalid_request_error", "Unsupported image request parameter")
		}
	}
	return req, nil
}

func loadedConfig() pluginConfig {
	if c, ok := currentConfig.Load().(pluginConfig); ok {
		return c
	}
	return pluginConfig{Enabled: true}
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	c := pluginConfig{Enabled: true}
	if err := yaml.Unmarshal(req.ConfigYAML, &c); err != nil {
		return err
	}
	if !filepath.IsAbs(c.RuntimeDir) {
		return fail(500, "configuration_error", "runtime_dir must be an absolute installed runtime path")
	}
	for _, name := range []string{".venv/bin/python", "worker.py", "upstream/services/openai_backend_api.py"} {
		if _, err := os.Stat(filepath.Join(c.RuntimeDir, name)); err != nil {
			return fail(500, "configuration_error", "Web image runtime is incomplete; run install-runtime.sh")
		}
	}
	currentConfig.Store(c)
	return nil
}

func registration() any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{Name: pluginID, Version: "0.3.0", Author: "Local", GitHubRepository: "https://github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin", ConfigFields: []pluginapi.ConfigField{
			{Name: "runtime_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Installed local web image runtime directory."},
			{Name: "route_gpt_image_2", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Route gpt-image-2 image requests through ChatGPT Web in addition to gpt-image-2-web."},
			{Name: "proxy_url", Type: pluginapi.ConfigFieldTypeString, Description: "Optional proxy for ChatGPT Web requests."},
		}},
		"capabilities": map[string]any{"model_provider": true, "model_router": true, "executor": true, "executor_model_scope": "static", "executor_input_formats": []string{"openai-image"}, "executor_output_formats": []string{"openai-image"}},
	}
}

func shouldRoute(req pluginapi.ModelRouteRequest, c pluginConfig) bool {
	return c.Enabled && req.SourceFormat == "openai-image" && (req.RequestedModel == modelID || (c.RouteGPTImage2 && req.RequestedModel == "gpt-image-2"))
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(registration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: []pluginapi.ModelInfo{{ID: modelID, Object: "model", OwnedBy: "openai", Type: "openai-image", DisplayName: "GPT Image 2 · ChatGPT Web", SupportedGenerationMethods: []string{"images"}, SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"image"}}}})
	case pluginabi.MethodModelRoute:
		var req pluginapi.ModelRouteRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: shouldRoute(req, loadedConfig()), TargetKind: pluginapi.ModelRouteTargetSelf})
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		return execute(raw)
	case pluginabi.MethodExecutorExecuteStream:
		return nil, fail(400, "invalid_request_error", "Web image generation currently requires stream=false")
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(map[string]any{})
	default:
		return nil, fail(400, "unsupported_method", "Unsupported web image plugin method")
	}
}

func selectCredential(callbackID string) (credential, string, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, map[string]string{"host_callback_id": callbackID})
	if err != nil {
		return credential{}, "", fail(503, "auth_unavailable", "Cannot read CPA credentials")
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return credential{}, "", fail(503, "auth_unavailable", "Invalid CPA credential list")
	}
	excluded := make(map[string]bool)
	for {
		entry, ok := accounts.reserve(list.Files, excluded)
		if !ok {
			return credential{}, "", fail(503, "auth_unavailable", "No idle, active free Codex credential with an unexpired token is available")
		}
		raw, errGet := hostCall(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": entry.AuthIndex, "host_callback_id": callbackID})
		var result pluginapi.HostAuthGetResponse
		if errGet == nil && json.Unmarshal(raw, &result) == nil {
			if c, valid := decodeCredential(result.JSON, time.Now()); valid {
				return c, entry.AuthIndex, nil
			}
		}
		accounts.release(entry.AuthIndex)
		excluded[entry.AuthIndex] = true
	}
}

func execute(raw []byte) ([]byte, error) {
	var rpc struct {
		pluginapi.ExecutorRequest
		CallbackID string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &rpc) != nil {
		return nil, fail(400, "invalid_request_error", "Invalid executor request")
	}
	c := loadedConfig()
	if !c.Enabled {
		return nil, fail(503, "plugin_disabled", "Web image plugin is disabled")
	}
	if rpc.SourceFormat != "openai-image" || rpc.Stream {
		return nil, fail(400, "invalid_request_error", "Only non-streaming image requests are supported")
	}
	payload := rpc.Payload
	if len(payload) == 0 {
		payload = rpc.OriginalRequest
	}
	path, _ := rpc.Metadata["request_path"].(string)
	req, err := parseImageRequest(payload, rpc.Headers.Get("Content-Type"), path)
	if err != nil {
		return nil, err
	}
	auth, index, err := selectCredential(rpc.CallbackID)
	if err != nil {
		return nil, err
	}
	defer accounts.release(index)
	if auth.ProxyURL != "" {
		c.ProxyURL = auth.ProxyURL
	}
	worker, err := startWorker(c, auth.AccessToken)
	if err != nil {
		return nil, err
	}
	defer worker.close()
	body, _ := json.Marshal(req)
	result, err := hostCall(pluginabi.MethodHostHTTPDo, map[string]any{
		"host_callback_id": rpc.CallbackID, "method": "POST", "url": worker.url + "/generate",
		"headers": http.Header{"Content-Type": []string{"application/json"}, "Authorization": []string{"Bearer " + worker.key}}, "body": body,
	})
	if err != nil {
		return nil, fail(502, "worker_transport_error", "Web image request was interrupted; generation may already have started")
	}
	var response pluginapi.HTTPResponse
	if json.Unmarshal(result, &response) != nil {
		return nil, fail(502, "worker_response_error", "Invalid web image worker response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, workerError(response.StatusCode, response.Body)
	}
	var images struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal(response.Body, &images) != nil || len(images.Data) != 1 || (images.Data[0].B64 == "" && images.Data[0].URL == "") {
		return nil, fail(502, "empty_image_result", "Web image generation returned no image")
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: http.Header{"Content-Type": []string{"application/json"}, "X-CPA-Image-Channel": []string{"chatgpt-web"}}})
}

type workerProcess struct {
	cmd      *exec.Cmd
	url, key string
	done     chan error
	once     sync.Once
}

func startWorker(c pluginConfig, accessToken string) (*workerProcess, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fail(500, "worker_start_error", "Cannot initialize worker authentication")
	}
	w := &workerProcess{key: hex.EncodeToString(secret), done: make(chan error, 1)}
	w.cmd = exec.Command(filepath.Join(c.RuntimeDir, ".venv/bin/python"), "-I", filepath.Join(c.RuntimeDir, "worker.py"))
	w.cmd.Dir = c.RuntimeDir
	w.cmd.Env = workerEnvironment(os.Environ())
	input, _ := json.Marshal(map[string]string{"access_token": accessToken, "worker_key": w.key, "proxy_url": c.ProxyURL})
	w.cmd.Stdin = bytes.NewReader(append(input, '\n'))
	w.cmd.Stderr = io.Discard
	stdout, err := w.cmd.StdoutPipe()
	if err != nil {
		return nil, fail(500, "worker_start_error", "Cannot initialize image worker")
	}
	workerMu.Lock()
	if stopping {
		workerMu.Unlock()
		return nil, fail(503, "plugin_stopping", "Web image plugin is stopping")
	}
	err = w.cmd.Start()
	if err == nil {
		workers[w.cmd] = struct{}{}
	}
	workerMu.Unlock()
	if err != nil {
		return nil, fail(500, "worker_start_error", "Cannot start image worker; check runtime installation")
	}
	go func() { w.done <- w.cmd.Wait(); close(w.done) }()
	reader := bufio.NewReaderSize(stdout, 4096)
	type readyLine struct {
		line []byte
		err  error
	}
	readyCh := make(chan readyLine, 1)
	go func() { line, errRead := reader.ReadSlice('\n'); readyCh <- readyLine{line, errRead} }()
	// This deadline covers local process startup only, before any upstream connection.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	var line []byte
	select {
	case result := <-readyCh:
		line, err = result.line, result.err
	case <-timer.C:
		w.close()
		return nil, fail(500, "worker_start_error", "Image worker did not initialize")
	}
	var ready struct {
		Port int `json:"port"`
	}
	if err != nil || len(line) > 4096 || json.Unmarshal(line, &ready) != nil || ready.Port < 1 || ready.Port > 65535 {
		w.close()
		return nil, fail(500, "worker_start_error", "Image worker failed to initialize")
	}
	w.url = fmt.Sprintf("http://127.0.0.1:%d", ready.Port)
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	return w, nil
}

func workerEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name == "PATH" || name == "HOME" || name == "TMPDIR" || name == "SYSTEMROOT" || name == "SSL_CERT_FILE" || name == "SSL_CERT_DIR" {
			result = append(result, entry)
		}
	}
	return append(result, "PYTHONDONTWRITEBYTECODE=1", "STORAGE_BACKEND=json")
}

func (w *workerProcess) close() {
	w.once.Do(func() {
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		<-w.done
		workerMu.Lock()
		delete(workers, w.cmd)
		workerMu.Unlock()
	})
}

func stopWorkers() {
	workerMu.Lock()
	defer workerMu.Unlock()
	stopping = true
	for cmd := range workers {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

func workerError(status int, body []byte) error {
	if status < 400 || status > 599 {
		status = 502
	}
	var data struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &data)
	// Worker errors are a fixed vocabulary; never expose upstream bodies or credentials.
	allowed := map[string]string{
		"image_quota_exhausted": "This account has no remaining web image quota; retry to select another account",
		"invalid_credential":    "The selected credential cannot authenticate to ChatGPT Web",
		"upstream_rejected":     "ChatGPT Web rejected the image request",
		"no_image_generated":    "ChatGPT Web completed without generating an image",
		"generation_failed":     "ChatGPT Web image generation failed; generation may already have started",
		"invalid_request_error": "Invalid web image request",
	}
	message, ok := allowed[data.Error.Code]
	if !ok {
		return fail(status, "generation_failed", allowed["generation_failed"])
	}
	return fail(status, data.Error.Code, message)
}

func fail(status int, code, message string) error { return pluginabi.NewError(code, message, status) }
func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}
func errorEnvelope(code, message string, status int) []byte {
	raw, _ := pluginabi.NewErrorEnvelope(code, message, status)
	return raw
}
func encodeError(err error) []byte {
	var e *pluginabi.Error
	if errors.As(err, &e) {
		return errorEnvelope(e.Code, e.Message, e.HTTPStatus)
	}
	return errorEnvelope("plugin_error", "Web image plugin failed", 500)
}
