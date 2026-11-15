package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxReferenceImages = 10
	maxImageBytes      = 10 << 20
	maxRequestBytes    = 32 << 20
	maxTextBytes       = 1 << 20
)

func parseImageRequest(raw []byte, contentType, path string) (imageRequest, error) {
	var empty imageRequest
	if len(raw) > maxRequestBytes {
		return empty, fail(413, "request_too_large", "Image request exceeds 32 MiB")
	}
	var fields map[string]json.RawMessage
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/") {
		var err error
		fields, err = parseImageMultipart(raw, contentType)
		if err != nil {
			return empty, err
		}
	} else if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return empty, fail(400, "invalid_request_error", "Invalid image request JSON")
	}
	if _, single := fields["image"]; single {
		if _, multiple := fields["images"]; multiple {
			return empty, fail(400, "invalid_request_error", "Use either image or images, not both")
		}
		fields["images"] = fields["image"]
		delete(fields, "image")
	}
	var images []string
	if input, exists := fields["images"]; exists {
		var entries []json.RawMessage
		if len(input) > 0 && input[0] == '[' {
			if json.Unmarshal(input, &entries) != nil {
				return empty, fail(400, "invalid_request_error", "Invalid reference images")
			}
		} else {
			entries = []json.RawMessage{input}
		}
		if len(entries) == 0 || len(entries) > maxReferenceImages {
			return empty, fail(400, "invalid_request_error", "Provide between 1 and 10 reference images")
		}
		for _, entry := range entries {
			value, err := imageValue(entry)
			if err != nil {
				return empty, err
			}
			image, err := normalizeImage(value)
			if err != nil {
				return empty, err
			}
			images = append(images, image)
		}
		fields["images"], _ = json.Marshal(images)
	}
	if strings.HasSuffix(path, "/images/edits") && len(images) == 0 {
		return empty, fail(400, "invalid_request_error", "Image edits require at least one reference image")
	}
	if strings.HasSuffix(path, "/images/generations") && len(images) > 0 {
		return empty, fail(400, "invalid_request_error", "Use /v1/images/edits for reference images")
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return empty, fail(400, "invalid_request_error", "Invalid image request")
	}
	if len(normalized) > maxRequestBytes {
		return empty, fail(413, "request_too_large", "Encoded image request exceeds 32 MiB")
	}
	req, err := validateImageRequest(normalized)
	if err == nil && len(req.Prompt) > maxTextBytes {
		return empty, fail(413, "request_too_large", "Image prompt exceeds 1 MiB")
	}
	return req, err
}

func imageValue(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) == nil && value != "" {
		return value, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, key := range []string{"image_url", "url", "b64_json"} {
			if v, ok := obj[key]; ok {
				if json.Unmarshal(v, &value) == nil {
					return value, nil
				}
				if key == "image_url" {
					var nested struct {
						URL string `json:"url"`
					}
					if json.Unmarshal(v, &nested) == nil {
						return nested.URL, nil
					}
				}
			}
		}
	}
	return "", fail(400, "invalid_request_error", "Reference images must contain base64 image data")
}

func normalizeImage(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "data:") {
		header, data, ok := strings.Cut(value, ",")
		if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
			return "", fail(400, "invalid_request_error", "Reference images require base64 data URLs")
		}
		value = data
	}
	if len(value) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return "", fail(413, "image_too_large", "Each reference image must be at most 10 MiB")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return "", fail(400, "invalid_request_error", "Use uploaded files, base64, or data URLs; remote URLs and local paths are not supported")
	}
	return imageDataURL(data)
}

func imageDataURL(data []byte) (string, error) {
	if len(data) > maxImageBytes {
		return "", fail(413, "image_too_large", "Each reference image must be at most 10 MiB")
	}
	mimeType := http.DetectContentType(data)
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
		return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	default:
		return "", fail(400, "invalid_request_error", "Reference images must be PNG, JPEG, or WebP")
	}
}

func parseImageMultipart(raw []byte, contentType string) (map[string]json.RawMessage, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, fail(400, "invalid_request_error", "Invalid multipart image request")
	}
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	fields := make(map[string]json.RawMessage)
	var images []string
	for {
		part, errPart := reader.NextPart()
		if errPart == io.EOF {
			break
		}
		if errPart != nil {
			return nil, fail(400, "invalid_request_error", "Invalid multipart image data")
		}
		name := part.FormName()
		isImage := name == "image" || name == "image[]"
		limit := int64(maxTextBytes)
		if isImage {
			limit = maxImageBytes
		}
		data, errRead := io.ReadAll(io.LimitReader(part, limit+1))
		if errRead != nil {
			return nil, fail(400, "invalid_request_error", "Cannot read multipart image data")
		}
		if int64(len(data)) > limit {
			return nil, fail(413, "request_too_large", "Multipart field exceeds its size limit")
		}
		if isImage {
			if len(images) >= maxReferenceImages {
				return nil, fail(400, "invalid_request_error", "Provide between 1 and 10 reference images")
			}
			value, errImage := imageDataURL(data)
			if errImage != nil {
				return nil, errImage
			}
			images = append(images, value)
			continue
		}
		if _, exists := fields[name]; exists {
			return nil, fail(400, "invalid_request_error", "Duplicate multipart field")
		}
		switch name {
		case "n":
			n, errN := strconv.Atoi(string(data))
			if errN != nil {
				return nil, fail(400, "invalid_request_error", "Invalid n")
			}
			fields[name], _ = json.Marshal(n)
		case "stream":
			stream, errStream := strconv.ParseBool(string(data))
			if errStream != nil {
				return nil, fail(400, "invalid_request_error", "Invalid stream")
			}
			fields[name], _ = json.Marshal(stream)
		case "model", "prompt", "size", "quality", "response_format", "user":
			fields[name], _ = json.Marshal(string(data))
		default:
			return nil, fail(400, "invalid_request_error", "Unsupported multipart field; masks are not supported")
		}
	}
	if len(images) > 0 {
		fields["images"], _ = json.Marshal(images)
	}
	return fields, nil
}
