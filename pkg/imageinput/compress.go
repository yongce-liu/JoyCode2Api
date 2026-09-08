package imageinput

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"strings"
)

const (
	// GatewayBodyLimit is the hard request-body limit enforced by the Color Gateway.
	GatewayBodyLimit = 5 << 20
	// SafeBodyTarget leaves room for metadata added after request preprocessing.
	SafeBodyTarget = GatewayBodyLimit - (128 << 10)

	preferredImageURLBytes       = 768 << 10
	maxDecodedImagePixels  int64 = 64 * 1024 * 1024
)

// Stats describes image preprocessing performed on one JSON request.
type Stats struct {
	OriginalBytes    int
	FinalBytes       int
	ImageCount       int
	CompressedImages int
}

type imageRef struct {
	current string
	apply   func(string)
	changed bool
}

// CompressJSONBody finds embedded base64 images in common OpenAI Responses,
// Chat Completions, and Anthropic Messages payloads. Large images are encoded
// as JPEG, and requests near the gateway limit are compressed more aggressively.
func CompressJSONBody(body []byte, targetBytes int) ([]byte, Stats, error) {
	stats := Stats{OriginalBytes: len(body), FinalBytes: len(body)}
	var root interface{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		// Let the protocol handler return its normal invalid-JSON response.
		return body, stats, nil
	}

	refs := collectImageRefs(root)
	stats.ImageCount = len(refs)
	if len(refs) == 0 {
		return body, stats, nil
	}

	// First apply a quality-preserving pass to every supported image, even when
	// the whole request is currently below the gateway limit.
	for index := range refs {
		budget := len(refs[index].current) - 1
		if budget > preferredImageURLBytes {
			budget = preferredImageURLBytes
		}
		compressed, err := compressImageDataURL(refs[index].current, budget)
		if err != nil || len(compressed) >= len(refs[index].current) {
			continue
		}
		refs[index].current = compressed
		refs[index].apply(compressed)
		refs[index].changed = true
	}

	processed, err := json.Marshal(root)
	if err != nil {
		return body, stats, fmt.Errorf("marshal image-compressed request: %w", err)
	}
	if len(processed) > targetBytes {
		if err := fitImageRefsToBodyLimit(root, refs, targetBytes); err != nil {
			return body, stats, err
		}
		processed, err = json.Marshal(root)
		if err != nil {
			return body, stats, fmt.Errorf("marshal size-fitted request: %w", err)
		}
	}

	for _, ref := range refs {
		if ref.changed {
			stats.CompressedImages++
		}
	}
	stats.FinalBytes = len(processed)
	if stats.CompressedImages == 0 {
		return body, stats, nil
	}
	if len(processed) > targetBytes {
		return body, stats, fmt.Errorf("request remains %d bytes after image compression; safe upstream budget is %d bytes", len(processed), targetBytes)
	}
	return processed, stats, nil
}

func fitImageRefsToBodyLimit(root interface{}, refs []imageRef, targetBytes int) error {
	currentURLBytes := 0
	for index := range refs {
		currentURLBytes += len(refs[index].current)
		refs[index].apply("")
	}
	withoutImages, err := json.Marshal(root)
	for index := range refs {
		refs[index].apply(refs[index].current)
	}
	if err != nil {
		return fmt.Errorf("measure request without images: %w", err)
	}

	availableURLBytes := targetBytes - len(withoutImages)
	if availableURLBytes <= 0 {
		return fmt.Errorf("request metadata and text use %d bytes, exceeding the safe %d-byte upstream budget before embedded images", len(withoutImages), targetBytes)
	}

	remainingBudget := availableURLBytes
	remainingCurrent := currentURLBytes
	for index := range refs {
		currentBytes := len(refs[index].current)
		budget := remainingBudget
		if index < len(refs)-1 && remainingCurrent > 0 {
			budget = remainingBudget * currentBytes / remainingCurrent
		}
		if budget < len("data:image/jpeg;base64,")+4 {
			return fmt.Errorf("embedded image budget is too small to fit %d images in the upstream request limit", len(refs))
		}

		if len(refs[index].current) > budget {
			compressed, err := compressImageDataURL(refs[index].current, budget)
			if err != nil {
				return fmt.Errorf("compress embedded image %d/%d: %w", index+1, len(refs), err)
			}
			refs[index].current = compressed
			refs[index].apply(compressed)
			refs[index].changed = true
		}
		remainingBudget -= len(refs[index].current)
		remainingCurrent -= currentBytes
	}
	return nil
}

func collectImageRefs(value interface{}) []imageRef {
	refs := make([]imageRef, 0)
	var walk func(interface{})
	walk = func(current interface{}) {
		switch typed := current.(type) {
		case map[string]interface{}:
			if typeName, _ := typed["type"].(string); typeName == "image" {
				if source, ok := typed["source"].(map[string]interface{}); ok {
					sourceType, _ := source["type"].(string)
					mediaType, _ := source["media_type"].(string)
					data, _ := source["data"].(string)
					if sourceType == "base64" && strings.HasPrefix(strings.ToLower(mediaType), "image/") && data != "" {
						refSource := source
						refs = append(refs, imageRef{
							current: "data:" + mediaType + ";base64," + data,
							apply: func(dataURL string) {
								mediaType, encoded := splitImageDataURL(dataURL)
								refSource["media_type"] = mediaType
								refSource["data"] = encoded
							},
						})
					}
				}
			}
			for key, child := range typed {
				if text, ok := child.(string); ok && isEmbeddedImageDataURL(text) {
					container, field := typed, key
					refs = append(refs, imageRef{
						current: text,
						apply:   func(dataURL string) { container[field] = dataURL },
					})
					continue
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return refs
}

func isEmbeddedImageDataURL(value string) bool {
	comma := strings.IndexByte(value, ',')
	if comma <= 0 {
		return false
	}
	header := strings.ToLower(value[:comma])
	return strings.HasPrefix(header, "data:image/") && strings.Contains(header, ";base64")
}

func splitImageDataURL(value string) (mediaType, encoded string) {
	comma := strings.IndexByte(value, ',')
	if comma <= 0 {
		return "", ""
	}
	header := value[:comma]
	if len(header) < len("data:") || !strings.EqualFold(header[:len("data:")], "data:") {
		return "", ""
	}
	mediaType = strings.SplitN(header[len("data:"):], ";", 2)[0]
	return mediaType, value[comma+1:]
}

func compressImageDataURL(value string, maxURLBytes int) (string, error) {
	_, encoded := splitImageDataURL(value)
	if encoded == "" {
		return "", fmt.Errorf("invalid image data URL")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode base64 image: %w", err)
		}
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode image metadata: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxDecodedImagePixels {
		return "", fmt.Errorf("image dimensions %dx%d exceed the safe decode limit", config.Width, config.Height)
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}

	const prefix = "data:image/jpeg;base64,"
	maxEncodedBytes := maxURLBytes - len(prefix)
	if maxEncodedBytes <= 4 {
		return "", fmt.Errorf("image budget %d bytes is too small", maxURLBytes)
	}
	maxJPEGBytes := (maxEncodedBytes / 4) * 3
	if maxJPEGBytes <= 0 {
		return "", fmt.Errorf("image budget %d bytes is too small", maxURLBytes)
	}

	bounds := decoded.Bounds()
	maxSide := bounds.Dx()
	if bounds.Dy() > maxSide {
		maxSide = bounds.Dy()
	}
	firstDimension := maxSide
	if firstDimension > 2048 {
		firstDimension = 2048
	}
	dimensions := []int{firstDimension}
	for _, candidate := range []int{1600, 1280, 1024, 768, 512, 384, 256} {
		if candidate < firstDimension {
			dimensions = append(dimensions, candidate)
		}
	}

	var smallest []byte
	for _, dimension := range dimensions {
		prepared := resizeImageOnWhite(decoded, dimension)
		for _, quality := range []int{88, 80, 70, 60, 50, 40, 30} {
			var output bytes.Buffer
			if err := jpeg.Encode(&output, prepared, &jpeg.Options{Quality: quality}); err != nil {
				return "", fmt.Errorf("encode JPEG: %w", err)
			}
			candidate := output.Bytes()
			if len(smallest) == 0 || len(candidate) < len(smallest) {
				smallest = append(smallest[:0], candidate...)
			}
			if len(candidate) <= maxJPEGBytes {
				return prefix + base64.StdEncoding.EncodeToString(candidate), nil
			}
		}
	}

	return "", fmt.Errorf("compressed image is still %d bytes, exceeding its %d-byte binary budget", len(smallest), maxJPEGBytes)
}

func resizeImageOnWhite(source image.Image, maxSide int) *image.RGBA {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	targetWidth, targetHeight := sourceWidth, sourceHeight
	if sourceWidth > maxSide || sourceHeight > maxSide {
		if sourceWidth >= sourceHeight {
			targetWidth = maxSide
			targetHeight = max(1, sourceHeight*maxSide/sourceWidth)
		} else {
			targetHeight = maxSide
			targetWidth = max(1, sourceWidth*maxSide/sourceHeight)
		}
	}

	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		sourceY := bounds.Min.Y + y*sourceHeight/targetHeight
		for x := 0; x < targetWidth; x++ {
			sourceX := bounds.Min.X + x*sourceWidth/targetWidth
			r, g, b, a := source.At(sourceX, sourceY).RGBA()
			// RGBA values are alpha-premultiplied. Composite transparent pixels on white.
			r += 0xffff - a
			g += 0xffff - a
			b += 0xffff - a
			target.SetRGBA(x, y, color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff})
		}
	}
	return target
}
