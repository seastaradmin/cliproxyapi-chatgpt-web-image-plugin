package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginabi"
	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginapi"
)

func TestRouteScope(t *testing.T) {
	for _, tc := range []struct {
		model, format        string
		enabled, alias, want bool
	}{
		{modelID, "openai-image", true, false, true},
		{"gpt-image-2", "openai-image", true, false, false},
		{"gpt-image-2", "openai-image", true, true, true},
		{modelID, "openai", true, true, false},
		{modelID, "openai-image", false, true, false},
		{"other", "openai-image", true, true, false},
	} {
		if got := shouldRoute(pluginapi.ModelRouteRequest{RequestedModel: tc.model, SourceFormat: tc.format}, pluginConfig{Enabled: tc.enabled, RouteGPTImage2: tc.alias}); got != tc.want {
			t.Errorf("route %+v = %v", tc, got)
		}
	}
}

func TestInvalidRequestsNeverReadCredentials(t *testing.T) {
	old := hostCall
	defer func() { hostCall = old }()
	hostCall = func(string, any) (json.RawMessage, error) { t.Fatal("unexpected host callback"); return nil, nil }
	for _, payload := range []string{
		`{"model":"gpt-image-2-web","prompt":""}`,
		`{"model":"gpt-image-2-web","prompt":"circle","n":2}`,
		`{"model":"gpt-image-2-web","prompt":"circle","stream":true}`,
		`{"model":"gpt-image-2-web","prompt":"circle","image":"x"}`,
		`{"model":"gpt-image-2-web","prompt":"circle","quality":4}`,
		`{"model":"gpt-image-2-web","prompt":"circle","response_format":"file"}`,
	} {
		raw, _ := json.Marshal(pluginapi.ExecutorRequest{Payload: []byte(payload), SourceFormat: "openai-image"})
		_, err := execute(raw)
		var pe *pluginabi.Error
		if !errors.As(err, &pe) || pe.HTTPStatus != 400 {
			t.Fatalf("expected 400, got %v", err)
		}
	}
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Payload: []byte(`{"model":"gpt-image-2-web","prompt":"circle"}`), SourceFormat: "openai-image", Metadata: map[string]any{"request_path": "/v1/images/edits"}})
	if _, err := execute(raw); err == nil {
		t.Fatal("edits must fail")
	}
}

func fakeJWT(payload string) string {
	return "test." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestCredentialEligibility(t *testing.T) {
	now := time.Unix(1000, 0)
	good := credential{Type: "codex", PlanType: "free", AccessToken: fakeJWT(`{"exp":2000}`)}
	for _, tc := range []struct {
		name   string
		change func(*credential)
		want   bool
	}{
		{"free", func(*credential) {}, true},
		{"paid", func(c *credential) { c.PlanType = "plus" }, false},
		{"disabled", func(c *credential) { c.Disabled = true }, false},
		{"expired", func(c *credential) { c.AccessToken = fakeJWT(`{"exp":1000}`) }, false},
		{"unknown", func(c *credential) { c.PlanType = "" }, false},
		{"claim", func(c *credential) {
			c.PlanType = ""
			c.IDToken = fakeJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"free"}}`)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.change(&c)
			raw, _ := json.Marshal(c)
			_, ok := decodeCredential(raw, now)
			if ok != tc.want {
				t.Fatalf("eligibility %v", ok)
			}
		})
	}
}

func TestAccountReservationAndRelease(t *testing.T) {
	p := accountPool{busy: map[string]bool{}}
	files := []pluginapi.HostAuthFileEntry{{AuthIndex: "a", Provider: "codex"}, {AuthIndex: "b", Type: "codex"}, {AuthIndex: "c", Provider: "codex", Disabled: true}}
	a, ok := p.reserve(files, nil)
	if !ok || a.AuthIndex != "a" {
		t.Fatal("first account")
	}
	b, ok := p.reserve(files, nil)
	if !ok || b.AuthIndex != "b" {
		t.Fatal("second account")
	}
	if _, ok = p.reserve(files, nil); ok {
		t.Fatal("must not share in-flight accounts")
	}
	p.release(a.AuthIndex)
	if a, ok = p.reserve(files, nil); !ok || a.AuthIndex != "a" {
		t.Fatal("released account unavailable")
	}
}

func TestErrorsDoNotExposeUpstreamBody(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"unexpected","message":"SECRET"}}`, `{"error":{"code":"invalid_credential","message":"SECRET"}}`, `SECRET`} {
		encoded := string(encodeError(workerError(403, []byte(body))))
		if strings.Contains(encoded, "SECRET") {
			t.Fatal("upstream secret leaked")
		}
	}
}

func fakeRuntime(t *testing.T, script string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	if err = os.MkdirAll(filepath.Join(dir, ".venv/bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(python, filepath.Join(dir, ".venv/bin/python")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "worker.py"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkerCleanupAndBoundedReadiness(t *testing.T) {
	dir := fakeRuntime(t, "import sys,time\nsys.stdin.readline()\nprint('{\"port\":12345}',flush=True)\ntime.sleep(60)\n")
	w, err := startWorker(pluginConfig{RuntimeDir: dir}, "private-test-token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(w.cmd.Args, " "), "private-test-token") || strings.Contains(strings.Join(w.cmd.Env, " "), "private-test-token") {
		t.Fatal("token in process metadata")
	}
	w.close()
	w.close()
	if w.cmd.ProcessState == nil || w.cmd.ProcessState.Success() {
		t.Fatal("worker not killed and reaped")
	}
	workerMu.Lock()
	count := len(workers)
	workerMu.Unlock()
	if count != 0 {
		t.Fatal("worker still registered")
	}
	dir = fakeRuntime(t, "import sys,time\nsys.stdin.readline()\nprint('x'*8192,flush=True)\ntime.sleep(60)\n")
	if w, err = startWorker(pluginConfig{RuntimeDir: dir}, "token"); err == nil {
		w.close()
		t.Fatal("oversized readiness accepted")
	}
}

func TestTransportCancellationReleasesWorkerAndAccount(t *testing.T) {
	oldHost := hostCall
	oldConfig := loadedConfig()
	defer func() { hostCall = oldHost; currentConfig.Store(oldConfig) }()
	dir := fakeRuntime(t, "import sys,time\nsys.stdin.readline()\nprint('{\"port\":12345}',flush=True)\ntime.sleep(60)\n")
	currentConfig.Store(pluginConfig{Enabled: true, RuntimeDir: dir})
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return json.RawMessage(`{"files":[{"auth_index":"cancel-test","provider":"codex"}]}`), nil
		case pluginabi.MethodHostAuthGet:
			c, _ := json.Marshal(credential{Type: "codex", PlanType: "free", AccessToken: fakeJWT(`{"exp":4102444800}`)})
			raw, _ := json.Marshal(pluginapi.HostAuthGetResponse{JSON: c})
			return raw, nil
		case pluginabi.MethodHostHTTPDo:
			return nil, errors.New("context canceled")
		default:
			t.Fatalf("unexpected callback %s", method)
			return nil, nil
		}
	}
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Payload: []byte(`{"model":"gpt-image-2-web","prompt":"circle"}`), SourceFormat: "openai-image"})
	if _, err := execute(raw); err == nil {
		t.Fatal("expected cancellation")
	}
	workerMu.Lock()
	count := len(workers)
	workerMu.Unlock()
	if count != 0 {
		t.Fatal("worker survived cancellation")
	}
	accounts.mu.Lock()
	busy := accounts.busy["cancel-test"]
	accounts.mu.Unlock()
	if busy {
		t.Fatal("account still busy")
	}
}
