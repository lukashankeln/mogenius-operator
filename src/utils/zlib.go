package utils

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"

	"encoding/json"
)

func ZlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	_, err := writer.Write(data)
	if err != nil {
		return nil, err
	}
	_ = writer.Close()

	return buf.Bytes(), nil
}

func ZlibDecompress(compressedData []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(compressedData)
	reader, err := zlib.NewReader(&buf)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	var outBuf bytes.Buffer
	_, err = io.Copy(&outBuf, reader)
	if err != nil {
		return nil, err
	}

	return outBuf.Bytes(), nil
}

// TryZlibDecompress decodes a zlib-compressed datagram payload. The payload is
// expected to be the base64 JSON string of the zlib bytes, mirroring how a
// []byte payload is encoded outbound by encoding/json. The result is the raw
// decompressed JSON, so it can be used directly as a json.RawMessage payload.
// An error is returned if the payload is not a base64 string, not valid zlib
// data, or does not decompress to valid JSON.
func TryZlibDecompress(data any) (json.RawMessage, error) {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}
	var compressed []byte
	if err := json.Unmarshal(dataBytes, &compressed); err != nil {
		return nil, fmt.Errorf("payload is not a base64 string: %w", err)
	}
	decompressed, err := ZlibDecompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("invalid zlib data: %w", err)
	}
	if !json.Valid(decompressed) {
		return nil, fmt.Errorf("decompressed payload is not valid JSON")
	}
	return json.RawMessage(decompressed), nil
}
