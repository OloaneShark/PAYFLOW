// demo-client is a local-only HTTPS smoke-test helper. It never contacts AWS
// directly and reads the synthetic merchant credential only from stdin.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const hostname = "payflow-demo.test"

type request struct {
	Path   string `json:"path"`
	Body   string `json:"body"`
	Bearer string `json:"bearer"`
}

func allowedPath(path string) bool {
	switch path {
	case "/readyz",
		"/payflow.v1.PaymentService/CreatePayment",
		"/payflow.v1.PaymentService/GetPayment",
		"/payflow.v1.PaymentService/SubmitPayment",
		"/payflow.v1.PaymentService/GetMerchantBalance":
		return true
	default:
		return false
	}
}

func run(certPath string, port int, input io.Reader, output io.Writer) error {
	if port < 1024 || port > 65535 {
		return errors.New("local port must be 1024..65535")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return errors.New("cannot read demo certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		return errors.New("invalid demo certificate")
	}
	var call request
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&call); err != nil {
		return errors.New("invalid request input")
	}
	if !allowedPath(call.Path) || (call.Path != "/readyz" && call.Bearer == "") {
		return errors.New("path or synthetic credential missing")
	}
	method := http.MethodGet
	if call.Path != "/readyz" {
		method = http.MethodPost
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+hostname+call.Path, bytes.NewBufferString(call.Body))
	if err != nil {
		return errors.New("cannot construct request")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+call.Bearer)
	}
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: hostname,
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("TLS or local tunnel request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}
	if _, err := io.Copy(output, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return errors.New("cannot read API response")
	}
	fmt.Fprintf(os.Stderr, "verified TLS for %s; HTTP %d\n", hostname, resp.StatusCode)
	return nil
}

func main() {
	certPath := flag.String("cert", "", "public demo certificate PEM")
	port := flag.Int("local-port", 18443, "loopback SSM tunnel port")
	flag.Parse()
	if err := run(*certPath, *port, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
