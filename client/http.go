package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// submitTx sends transaction bytes to a node via POST /tx.
func submitTx(nodeAddr string, txBytes []byte) error {
	resp, err := http.Post(
		"http://"+nodeAddr+"/tx",
		"application/octet-stream",
		bytes.NewReader(txBytes),
	)
	if err != nil {
		return fmt.Errorf("post tx:\n%w", err)
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("tx rejected: status %d", resp.StatusCode)
	}

	return nil
}

// httpGet performs a GET request and decodes the JSON response.
func httpGet(url string, result any) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s:\n%w", url, err)
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

// httpPostJSON performs a POST request with JSON body and decodes the JSON response.
func httpPostJSON(url string, body any, result any) error {
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body:\n%w", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("POST %s:\n%w", url, err)
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST %s: status %d", url, resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(result)
}
