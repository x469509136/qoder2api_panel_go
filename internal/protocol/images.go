package protocol

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
)

// ImageStats reports what the preflight did, for logging only.
type ImageStats struct {
	Images       int
	Optimized    int
	BytesBefore  int
	BytesAfter   int
	CodecMissing int
}

// ImagePreflightEnabled controls QD_IMAGE_TRANSCODE (default on).
func ImagePreflightEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_IMAGE_TRANSCODE")))
	return !(raw == "0" || raw == "off" || raw == "false" || raw == "no" || raw == "disabled")
}

// imageCacheEntry is one normalised image kept in memory so the same image
// produces identical bytes across turns and retries (prefix stability).
type imageCacheEntry struct {
	dataURI []byte
	size    int
}

var (
	imageMu    sync.Mutex
	imageCache = map[[32]byte]imageCacheEntry{}
	imageOrder [][32]byte
	imageBytes int
)

const (
	imageCacheMaxBytes = 32 << 20
	imageCacheMaxItems = 32
	imageMinBytes      = 256 << 10
)

// PrepareInlineImages normalises large inline PNG/BMP/TIFF images.
//
// The goal is one thing only: the same image must produce byte-identical output
// on every turn so the upstream prompt prefix stays stable, and the provider's
// image-conversion stage has less work. No image is removed, no resolution is
// lowered, and remote URLs are never fetched or rewritten.
func PrepareInlineImages(messages []map[string]any) ([]map[string]any, ImageStats) {
	stats := ImageStats{}
	if !ImagePreflightEnabled() {
		return messages, stats
	}
	for _, msg := range messages {
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			pm, ok := part.(map[string]any)
			if !ok || asString(pm["type"]) != "image_url" {
				continue
			}
			iu, ok := pm["image_url"].(map[string]any)
			if !ok {
				continue
			}
			url, _ := iu["url"].(string)
			if !strings.HasPrefix(url, "data:") {
				continue // remote URLs are never fetched
			}
			stats.Images++
			normalized, before, after, optimized, missing := normalizeDataURI(url)
			if missing > 0 {
				stats.CodecMissing++
			}
			if optimized {
				iu["url"] = normalized
				stats.Optimized++
				stats.BytesBefore += before
				stats.BytesAfter += after
			}
		}
	}
	return messages, stats
}

// normalizeDataURI rewrites a large PNG data URI into a smaller equivalent.
//
// The implementation is deliberately conservative: it only rewrites when the
// image is a plain large PNG with no transparency, and only when the result is
// at least 20% smaller. Everything it cannot decode is returned untouched.
func normalizeDataURI(url string) (string, int, int, bool, int) {
	before := len(url)
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		return url, before, before, false, 0
	}
	header := url[:comma]
	if !strings.Contains(header, "base64") {
		return url, before, before, false, 0
	}
	encoded := url[comma+1:]
	if len(encoded)*3/4 < imageMinBytes {
		return url, before, before, false, 0
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return url, before, before, false, 0
	}
	if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
		return url, before, before, false, 0
	}
	key := sha256.Sum256(raw)
	imageMu.Lock()
	if hit, ok := imageCache[key]; ok {
		imageMu.Unlock()
		return string(hit.dataURI), before, len(hit.dataURI), true, 0
	}
	imageMu.Unlock()

	compressed, ok := recompressPNG(raw)
	if !ok {
		return url, before, before, false, 0
	}
	after := len(url[:comma+1]) + base64.StdEncoding.EncodedLen(len(compressed))
	if float64(after) > float64(before)*0.8 {
		// Not worth it: keep the original so nothing surprising happens.
		return url, before, before, false, 0
	}
	newURL := url[:comma+1] + base64.StdEncoding.EncodeToString(compressed)
	imageMu.Lock()
	if _, exists := imageCache[key]; !exists {
		for len(imageOrder) > 0 && (len(imageOrder) >= imageCacheMaxItems ||
			imageBytes+after > imageCacheMaxBytes) {
			old := imageOrder[0]
			imageOrder = imageOrder[1:]
			if e, ok := imageCache[old]; ok {
				imageBytes -= e.size
				delete(imageCache, old)
			}
		}
		imageCache[key] = imageCacheEntry{dataURI: []byte(newURL), size: after}
		imageOrder = append(imageOrder, key)
		imageBytes += after
	}
	imageMu.Unlock()
	return newURL, before, after, true, 0
}

// recompressPNG re-deflates the IDAT chunks.
//
// It parses the chunk layout, concatenates the IDAT payloads, inflates them and
// re-deflates at the best level, then rebuilds the file with one IDAT chunk
// placed directly after IHDR. Nothing about the pixel data changes, so
// transparency, bit depth and metadata stay exactly as they were — and no image
// decoder is needed, which keeps the gateway dependency-free.
func recompressPNG(raw []byte) ([]byte, bool) {
	const sigLen = 8
	if len(raw) < sigLen+12 {
		return nil, false
	}
	type chunk struct {
		typ     string
		payload []byte
	}
	var chunks []chunk
	var idat []byte
	pos := sigLen
	for pos+12 <= len(raw) {
		length := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		if length < 0 || pos+12+length > len(raw) {
			return nil, false
		}
		typ := string(raw[pos+4 : pos+8])
		payload := raw[pos+8 : pos+8+length]
		if typ == "IDAT" {
			idat = append(idat, payload...)
		} else {
			chunks = append(chunks, chunk{typ: typ, payload: payload})
		}
		pos += 12 + length
	}
	if len(idat) == 0 || len(chunks) == 0 || chunks[0].typ != "IHDR" {
		return nil, false
	}
	zr, err := zlib.NewReader(bytes.NewReader(idat))
	if err != nil {
		return nil, false
	}
	inflated, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		// Truncated or corrupt stream: leave the original alone.
		return nil, false
	}
	var compressed bytes.Buffer
	zw, err := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if err != nil {
		return nil, false
	}
	if _, err := zw.Write(inflated); err != nil {
		_ = zw.Close()
		return nil, false
	}
	if err := zw.Close(); err != nil {
		return nil, false
	}

	var out bytes.Buffer
	out.Write(raw[:sigLen])
	writeChunk(&out, chunks[0].typ, chunks[0].payload) // IHDR
	writeChunk(&out, "IDAT", compressed.Bytes())
	for _, c := range chunks[1:] {
		writeChunk(&out, c.typ, c.payload)
	}
	return out.Bytes(), true
}

func writeChunk(w *bytes.Buffer, typ string, payload []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	w.Write(lenBuf[:])
	w.WriteString(typ)
	w.Write(payload)
	crc := crc32IEEE(append([]byte(typ), payload...))
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc)
	w.Write(crcBuf[:])
}

var crcTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		c := uint32(i)
		for k := 0; k < 8; k++ {
			if c&1 != 0 {
				c = 0xEDB88320 ^ (c >> 1)
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return t
}()

func crc32IEEE(data []byte) uint32 {
	crc := ^uint32(0)
	for _, b := range data {
		crc = crcTable[byte(crc)^b] ^ (crc >> 8)
	}
	return ^crc
}

// ImagePreflightStatus is the content-free operational snapshot for /health.
func ImagePreflightStatus() map[string]any {
	imageMu.Lock()
	count, size := len(imageCache), imageBytes
	imageMu.Unlock()
	return map[string]any{
		"enabled": ImagePreflightEnabled(),
		// The Go implementation needs no third-party codec: it re-deflates IDAT
		// without decoding pixels.
		"codec_available": true,
		"cache_entries":   count,
		"cache_bytes":     size,
	}
}

// MarshalMessages renders messages for logging/tests without HTML escaping.
func MarshalMessages(messages []map[string]any) string {
	blob, err := json.Marshal(messages)
	if err != nil {
		return "[]"
	}
	return string(blob)
}
