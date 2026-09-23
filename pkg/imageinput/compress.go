// Package imageinput optionally reduces inline image inputs before forwarding.
package imageinput

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// Prepare handles OpenAI data URLs and Anthropic base64 blocks, including images
// inside tool results. Remote URLs and unsupported formats are left untouched.
func Prepare(r *http.Request, s *store.Store) {
	if s == nil || s.GetSetting("auto_compress_images") != "true" {
		return
	}
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return
	}
	var body map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		return
	}
	size := s.GetIntSetting("image_max_dimension", 1600)
	if size < 256 || size > 4096 {
		size = 1600
	}
	quality := s.GetIntSetting("image_jpeg_quality", 80)
	if quality < 1 || quality > 100 {
		quality = 80
	}
	changed := false
	messages, _ := body["messages"].([]interface{})
	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok {
			changed = rewriteContent(m["content"], size, quality) || changed
		}
	}
	if !changed {
		return
	}
	if data, err := json.Marshal(body); err == nil {
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
	}
}

func rewriteContent(content interface{}, size, quality int) bool {
	changed := false
	blocks, _ := content.([]interface{})
	for _, block := range blocks {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		switch b["type"] {
		case "image_url":
			img, _ := b["image_url"].(map[string]interface{})
			url, _ := img["url"].(string)
			head, data, ok := strings.Cut(url, ",")
			if !ok || !strings.HasPrefix(head, "data:image/") || !strings.HasSuffix(head, ";base64") {
				continue
			}
			if compressed, mime, ok := compress(data, size, quality); ok {
				img["url"] = "data:" + mime + ";base64," + compressed
				changed = true
			}
		case "image":
			source, _ := b["source"].(map[string]interface{})
			if source["type"] != "base64" {
				continue
			}
			data, _ := source["data"].(string)
			if compressed, mime, ok := compress(data, size, quality); ok {
				source["data"] = compressed
				source["media_type"] = mime
				changed = true
			}
		case "tool_result":
			changed = rewriteContent(b["content"], size, quality) || changed
		}
	}
	return changed
}

func compress(encoded string, maxSide, quality int) (string, string, bool) {
	// Bound decoder allocations; skip animated/unsupported inputs instead of
	// flattening frames or changing content that the decoder cannot preserve.
	if len(encoded) > 28<<20 {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 20_000_000 {
		return "", "", false
	}
	if format == "png" && bytes.Contains(raw, []byte("acTL")) {
		return "", "", false
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", "", false
	}
	w, h := cfg.Width, cfg.Height
	if w > maxSide || h > maxSide {
		if w >= h {
			h = max(1, h*maxSide/w)
			w = maxSide
		} else {
			w = max(1, w*maxSide/h)
			h = maxSide
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		// Bilinear interpolation in premultiplied RGBA preserves transparent edges.
		for y := 0; y < h; y++ {
			sy := float64(y) * float64(cfg.Height-1) / float64(max(1, h-1))
			y0 := int(sy)
			fy := sy - float64(y0)
			for x := 0; x < w; x++ {
				sx := float64(x) * float64(cfg.Width-1) / float64(max(1, w-1))
				x0 := int(sx)
				fx := sx - float64(x0)
				r0, g0, b0, a0 := src.At(x0, y0).RGBA()
				r1, g1, b1, a1 := src.At(min(x0+1, cfg.Width-1), y0).RGBA()
				r2, g2, b2, a2 := src.At(x0, min(y0+1, cfg.Height-1)).RGBA()
				r3, g3, b3, a3 := src.At(min(x0+1, cfg.Width-1), min(y0+1, cfg.Height-1)).RGBA()
				mix := func(a, b, c, d uint32) uint8 {
					return uint8(((1-fy)*((1-fx)*float64(a)+fx*float64(b)) + fy*((1-fx)*float64(c)+fx*float64(d))) / 257)
				}
				dst.SetRGBA(x, y, color.RGBA{R: mix(r0, r1, r2, r3), G: mix(g0, g1, g2, g3), B: mix(b0, b1, b2, b3), A: mix(a0, a1, a2, a3)})
			}
		}
		src = dst
	}
	var out bytes.Buffer
	mime := "image/jpeg"
	if opaque, ok := src.(interface{ Opaque() bool }); ok && !opaque.Opaque() {
		mime = "image/png"
		err = png.Encode(&out, src)
	} else {
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: quality})
	}
	if err != nil || out.Len() >= len(raw) {
		return "", "", false
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), mime, true
}
