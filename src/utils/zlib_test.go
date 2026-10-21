package utils_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"mogenius-operator/src/utils"

	"github.com/stretchr/testify/assert"
)

func mustCompress(t *testing.T, b []byte) []byte {
	t.Helper()
	c, err := utils.ZlibCompress(b)
	assert.NoError(t, err)
	return c
}

func TestTryZlibDecompress(t *testing.T) {
	original := []byte(`{"name":"x","count":3}`)
	compressed := mustCompress(t, original)

	envelope, err := json.Marshal(map[string]any{"payload": compressed})
	assert.NoError(t, err)
	var parsed struct {
		Payload json.RawMessage `json:"payload"`
	}
	assert.NoError(t, json.Unmarshal(envelope, &parsed))

	type result struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	successes := []struct {
		name  string
		input any
	}{
		{"outbound envelope raw message", parsed.Payload},
		{"base64 string", base64.StdEncoding.EncodeToString(compressed)},
		{"raw bytes", compressed},
	}
	for _, tt := range successes {
		t.Run(tt.name, func(t *testing.T) {
			out, err := utils.TryZlibDecompress(tt.input)
			assert.NoError(t, err)
			assert.JSONEq(t, string(original), string(out))
			var r result
			assert.NoError(t, json.Unmarshal(out, &r))
			assert.Equal(t, result{Name: "x", Count: 3}, r)
		})
	}

	failures := []struct {
		name  string
		input any
	}{
		{"nil payload", nil},
		{"json object payload", json.RawMessage(`{"a":1}`)},
		{"base64 of non-zlib bytes", []byte("definitely not zlib")},
		{"zlib of non-json bytes", mustCompress(t, []byte("not json"))},
		{"zlib of empty input", mustCompress(t, []byte{})},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			out, err := utils.TryZlibDecompress(tt.input)
			assert.Error(t, err)
			assert.Nil(t, out)
		})
	}
}
