package imageinput

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestCompressJSONBodyProcessesResponsesDataURLs(t *testing.T) {
	originalURL := testPNGDataURL(t, 640, 480, 1)
	request := map[string]interface{}{
		"model": "test",
		"input": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type": "input_image", "image_url": originalURL, "detail": "high",
			}},
		}},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	processed, stats, err := CompressJSONBody(body, SafeBodyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ImageCount != 1 || stats.CompressedImages != 1 || stats.FinalBytes >= stats.OriginalBytes {
		t.Fatalf("unexpected stats: %#v", stats)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(processed, &result); err != nil {
		t.Fatal(err)
	}
	refs := collectImageRefs(result)
	if len(refs) != 1 || !strings.HasPrefix(refs[0].current, "data:image/jpeg;base64,") {
		t.Fatalf("image was not converted to JPEG: %#v", refs)
	}
}

func TestCompressJSONBodyProcessesAnthropicBase64Sources(t *testing.T) {
	dataURL := testPNGDataURL(t, 640, 480, 2)
	_, encoded := splitImageDataURL(dataURL)
	request := map[string]interface{}{
		"model": "test",
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": encoded,
				},
			}},
		}},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	processed, stats, err := CompressJSONBody(body, SafeBodyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ImageCount != 1 || stats.CompressedImages != 1 {
		t.Fatalf("unexpected stats: %#v", stats)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(processed, &result); err != nil {
		t.Fatal(err)
	}
	messages := result["messages"].([]interface{})
	content := messages[0].(map[string]interface{})["content"].([]interface{})
	source := content[0].(map[string]interface{})["source"].(map[string]interface{})
	if source["media_type"] != "image/jpeg" {
		t.Fatalf("media_type = %#v", source["media_type"])
	}
	if source["data"] == encoded {
		t.Fatal("base64 image data was not replaced")
	}
}

func TestCompressJSONBodyFitsMultipleImagesUnderGatewayTarget(t *testing.T) {
	first := testPNGDataURL(t, 900, 900, 11)
	second := testPNGDataURL(t, 900, 900, 12)
	request := map[string]interface{}{
		"model": "test",
		"input": []interface{}{map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "input_image", "image_url": first},
				map[string]interface{}{"type": "input_image", "image_url": second},
			},
		}},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= GatewayBodyLimit {
		t.Fatalf("test request = %d bytes, expected more than %d", len(body), GatewayBodyLimit)
	}

	processed, stats, err := CompressJSONBody(body, SafeBodyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(processed) > SafeBodyTarget {
		t.Fatalf("processed request = %d bytes, target = %d", len(processed), SafeBodyTarget)
	}
	if stats.ImageCount != 2 || stats.CompressedImages == 0 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
}

func TestCompressJSONBodyLeavesInvalidJSONForProtocolHandler(t *testing.T) {
	body := []byte(`{"input":`)
	processed, stats, err := CompressJSONBody(body, SafeBodyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(processed, body) || stats.CompressedImages != 0 {
		t.Fatalf("invalid JSON was modified: %#v", stats)
	}
}

func testPNGDataURL(t *testing.T, width, height, seed int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	state := uint32(seed)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			state = state*1664525 + 1013904223
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(state >> 24),
				G: uint8(state >> 16),
				B: uint8(state >> 8),
				A: 0xff,
			})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(output.Bytes())
}
