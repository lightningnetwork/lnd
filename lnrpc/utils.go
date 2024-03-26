package lnrpc

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// FileExists reports whether the named file or directory exists.
func FileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// IsValidURL checks if the given text is a valid URL.
// Passed text must have a scheme of "http" or "https" format to be valid.
func IsValidURL(text string) bool {
	parsedURL, err := url.Parse(text)
	if err != nil {
		return false
	}

	// Consider it a valid URL if it has a scheme of http or https.
	return parsedURL.Scheme == "http" || parsedURL.Scheme == "https"
}

// FetchURL returns the content fetched from the specified URL in bytes.
func FetchURL(url string) ([]byte, error) {
	// Perform the HTTP GET request.
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching URL failed: %w", err)
	}
	defer resp.Body.Close()

	// Check the HTTP response status.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code: %d", resp.StatusCode)
	}

	// Read the response body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body failed: %w", err)
	}

	return body, nil
}
