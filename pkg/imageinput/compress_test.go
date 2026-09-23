package imageinput

import (
	"bytes"
	"encoding/base64"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompressionPreservesGeometryAndTransparency(t *testing.T) {
	for _, transparent := range []bool{false, true} {
		src := image.NewNRGBA(image.Rect(0, 0, 800, 400))
		for y := 0; y < 400; y++ {
			for x := 0; x < 800; x++ {
				a := uint8(255)
				if transparent && x < 400 {
					a = 128
				}
				src.SetNRGBA(x, y, color.NRGBA{uint8(x*7 + y), uint8(x + y*13), uint8(x * y), a})
			}
		}
		var b bytes.Buffer
		png.Encode(&b, src)
		out, mime, ok := compress(base64.StdEncoding.EncodeToString(b.Bytes()), 256, 80)
		if !ok {
			t.Fatal("not compressed")
		}
		raw, err := base64.StdEncoding.DecodeString(out)
		if err != nil {
			t.Fatal(err)
		}
		decoded, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Bounds().Dx() != 256 || decoded.Bounds().Dy() != 128 || len(raw) >= b.Len() {
			t.Fatalf("dimensions/size incorrect")
		}
		if transparent {
			if mime != "image/png" {
				t.Fatal("lost transparency")
			}
			_, _, _, a := decoded.At(1, 1).RGBA()
			if a == 65535 {
				t.Fatal("became opaque")
			}
		}
	}
}
func TestUnsupportedImagesAndURLsUnchanged(t *testing.T) {
	for _, raw := range []string{"bad base64", base64.StdEncoding.EncodeToString([]byte("not an image"))} {
		if _, _, ok := compress(raw, 1600, 80); ok {
			t.Fatal("invalid image changed")
		}
	}
	content := []interface{}{map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/test.png"}}}
	rewriteContent(content, 1600, 80)
	if content[0].(map[string]interface{})["image_url"].(map[string]interface{})["url"] != "https://example.com/test.png" {
		t.Fatal("remote URL changed")
	}
}

func TestInlineBlockFormats(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			src.SetNRGBA(x, y, color.NRGBA{uint8(x * y), uint8(x + y), uint8(x*3 + y*7), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, src)
	encoded := base64.StdEncoding.EncodeToString(b.Bytes())
	openai := map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64," + encoded, "detail": "high"}}
	anthropic := map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": encoded}}
	rewriteContent([]interface{}{openai, map[string]interface{}{"type": "tool_result", "content": []interface{}{anthropic}}}, 256, 80)
	url := openai["image_url"].(map[string]interface{})
	if url["url"] == "data:image/png;base64,"+encoded || url["detail"] != "high" {
		t.Fatal("OpenAI compression lost detail or did not compress")
	}
	source := anthropic["source"].(map[string]interface{})
	if source["data"] == encoded || source["media_type"] != "image/jpeg" {
		t.Fatal("Anthropic tool-result image not compressed")
	}
}

func TestPreparePreservesLargeIntegerAndDisabledBody(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := image.NewNRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			src.SetNRGBA(x, y, color.NRGBA{uint8(x * y), uint8(x + y), uint8(x*3 + y*7), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, src)
	raw := `{"seed":9007199254740993,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(b.Bytes()) + `"}}]}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw))
	Prepare(r, st)
	data, _ := io.ReadAll(r.Body)
	if string(data) != raw {
		t.Fatal("disabled compression changed body")
	}
	st.SetSetting("auto_compress_images", "true")
	st.SetSetting("image_max_dimension", "256")
	r = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw))
	Prepare(r, st)
	data, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(data), "9007199254740993") || len(data) >= len(raw) {
		t.Fatal("large integer lost or image not compressed")
	}
}
