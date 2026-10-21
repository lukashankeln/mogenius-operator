package structs

import (
	"testing"

	"mogenius-operator/src/utils"

	"github.com/stretchr/testify/assert"
)

func TestPayloadMapAfterTryZlibDecompress(t *testing.T) {
	compressed, err := utils.ZlibCompress([]byte(`{"name":"x","count":3}`))
	assert.NoError(t, err)

	out, err := utils.TryZlibDecompress(compressed)
	assert.NoError(t, err)

	d := Datagram{Payload: out}
	assert.Equal(t, map[string]any{"name": "x", "count": float64(3)}, d.PayloadMap())
}
