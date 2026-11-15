package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"testing"

	"github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/internal/cpasdk/pluginabi"
)

func referencePNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, c)
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestJSONReferencesPreserveOrderAndBytes(t *testing.T) {
	first := referencePNG(t, color.Black)
	second := referencePNG(t, color.White)
	a := "data:image/png;base64," + base64.StdEncoding.EncodeToString(first)
	b := base64.StdEncoding.EncodeToString(second)
	for _, refs := range []any{[]string{a, b}, []any{map[string]string{"image_url": a}, map[string]any{"image_url": map[string]string{"url": b}}}} {
		raw, _ := json.Marshal(map[string]any{"model": modelID, "prompt": "Combine the references", "images": refs, "size": "2048x2048", "quality": "high"})
		req, err := parseImageRequest(raw, "application/json", "/v1/images/edits")
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Images) != 2 || req.Images[0] != a || req.Images[1] != "data:image/png;base64,"+b {
			t.Fatal("reference order or bytes changed")
		}
		if req.Size != "2048x2048" || req.Quality != "high" {
			t.Fatal("image hints changed")
		}
	}
	raw, _ := json.Marshal(map[string]any{"model": modelID, "prompt": "Edit this", "image": map[string]string{"b64_json": b}})
	req, err := parseImageRequest(raw, "application/json", "/v1/images/edits")
	if err != nil || len(req.Images) != 1 {
		t.Fatalf("single reference failed: %v", err)
	}
}

func TestMultipartReferencesPreserveOrderAndBytes(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, field := range [][2]string{{"model", modelID}, {"prompt", "Combine both"}, {"n", "1"}, {"stream", "false"}} {
		if err := w.WriteField(field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	inputs := [][]byte{referencePNG(t, color.Black), referencePNG(t, color.White)}
	for _, data := range inputs {
		part, err := w.CreateFormFile("image[]", "reference.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := parseImageRequest(body.Bytes(), w.FormDataContentType(), "/v1/images/edits")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Images) != 2 || req.N == nil || *req.N != 1 {
		t.Fatal("multipart fields missing")
	}
	for i, data := range inputs {
		if req.Images[i] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
			t.Fatal("multipart reference changed")
		}
	}
}

func TestInvalidReferences(t *testing.T) {
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(referencePNG(t, color.Black))
	for _, refs := range []any{[]string{}, []string{"/etc/passwd"}, []string{"https://example.com/image.png"}, []string{"data:image/png;base64,broken"}, []string{base64.StdEncoding.EncodeToString([]byte("not an image"))}, make([]string, 11)} {
		raw, _ := json.Marshal(map[string]any{"model": modelID, "prompt": "Edit", "images": refs})
		_, err := parseImageRequest(raw, "application/json", "/v1/images/edits")
		var pe *pluginabi.Error
		if !errors.As(err, &pe) || pe.HTTPStatus != 400 {
			t.Fatalf("invalid references accepted: %v", err)
		}
	}
	for _, fields := range []map[string]any{
		{"image": data, "images": []string{data}},
		{"images": []string{data}, "mask": data},
	} {
		fields["model"] = modelID
		fields["prompt"] = "Edit"
		raw, _ := json.Marshal(fields)
		if _, err := parseImageRequest(raw, "application/json", "/v1/images/edits"); err == nil {
			t.Fatal("ambiguous or unsupported input accepted")
		}
	}
	raw, _ := json.Marshal(map[string]any{"model": modelID, "prompt": "Edit", "images": []string{data}})
	if _, err := parseImageRequest(raw, "application/json", "/v1/images/generations"); err == nil {
		t.Fatal("references must use edits")
	}
	if _, err := imageDataURL(make([]byte, maxImageBytes+1)); err == nil {
		t.Fatal("oversize reference accepted")
	}
	if _, err := parseImageRequest(make([]byte, maxRequestBytes+1), "application/json", "/v1/images/edits"); err == nil {
		t.Fatal("oversize body accepted")
	}
	if _, err := parseImageRequest([]byte("broken"), "multipart/form-data", "/v1/images/edits"); err == nil {
		t.Fatal("missing multipart boundary accepted")
	}
}
