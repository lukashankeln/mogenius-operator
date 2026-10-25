package utils

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFetchCountrySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"DE","name":"Germany","isEuMember":true}`))
	}))
	t.Cleanup(server.Close)

	country, err := fetchCountry(server.Client(), server.URL)

	assert.NoError(t, err)
	if assert.NotNil(t, country) {
		assert.Equal(t, "DE", country.Code)
		assert.Equal(t, "Germany", country.Name)
		assert.True(t, country.IsEuMember)
	}
}

func TestFetchCountryNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	country, err := fetchCountry(server.Client(), server.URL)

	assert.ErrorContains(t, err, "500")
	assert.Nil(t, country)
}

func TestFetchCountryMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(server.Close)

	country, err := fetchCountry(server.Client(), server.URL)

	assert.Error(t, err)
	assert.Nil(t, country)
}

func TestFetchCountryTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// cleanups run LIFO: release the handler first, then Close can finish
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	client := &http.Client{Timeout: 50 * time.Millisecond}
	start := time.Now()
	country, err := fetchCountry(client, server.URL)
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.Nil(t, country)
	assert.Less(t, elapsed, 2*time.Second)
}

// GuessClusterCountry runs under the cluster/resource-info TTLCache mutex,
// so the production client must never be unbounded.
func TestCountryLookupClientHasTimeout(t *testing.T) {
	assert.Equal(t, 10*time.Second, countryLookupClient.Timeout)
}
