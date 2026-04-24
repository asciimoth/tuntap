//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/asciimoth/gonnect/tun"
	"github.com/asciimoth/tuntap"
)

const (
	leftTunName  = "tunload0"
	rightTunName = "tunload1"
	leftIP       = "10.232.0.1"
	rightIP      = "10.232.0.2"
	prefix       = 24
	listenPort   = 18080
	tunMTU       = 1500
)

var (
	workers           = flag.Int("workers", 8, "number of parallel download workers")
	requestsPerWorker = flag.Int("requests-per-worker", 4, "requests per worker")
	responseMiB       = flag.Int("response-mib", 16, "response size per request in MiB")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Lmicroseconds | log.Lshortfile)

	if !isAdmin() {
		log.Fatal("this example must be run as Administrator")
	}

	leftTun, err := tuntap.CreateTUN(leftTunName, tunMTU)
	if err != nil {
		log.Fatalf("create left TUN: %v", err)
	}
	defer leftTun.Close()

	rightTun, err := tuntap.CreateTUN(rightTunName, tunMTU)
	if err != nil {
		log.Fatalf("create right TUN: %v", err)
	}
	defer rightTun.Close()

	leftActual, err := leftTun.Name()
	if err != nil {
		log.Fatalf("left TUN name: %v", err)
	}
	rightActual, err := rightTun.Name()
	if err != nil {
		log.Fatalf("right TUN name: %v", err)
	}

	leftIdx, err := getInterfaceIndex(leftActual)
	if err != nil {
		log.Fatalf("left interface index: %v", err)
	}
	rightIdx, err := getInterfaceIndex(rightActual)
	if err != nil {
		log.Fatalf("right interface index: %v", err)
	}

	configureInterface(leftActual, leftIP, leftIdx, rightIP)
	defer cleanupInterface(leftActual, leftIdx, leftIP, rightIP)
	configureInterface(rightActual, rightIP, rightIdx, leftIP)
	defer cleanupInterface(rightActual, rightIdx, rightIP, leftIP)

	go logEvents("left", leftTun.Events())
	go logEvents("right", rightTun.Events())

	copyErrCh := make(chan error, 1)
	go func() {
		copyErrCh <- tun.Copy(leftTun, rightTun)
	}()

	server, serverErrCh, err := startHTTPServer()
	if err != nil {
		log.Fatalf("start server: %v", err)
	}

	if err := waitForServer(); err != nil {
		log.Fatalf("wait for server: %v", err)
	}

	if err := runClient(); err != nil {
		log.Fatalf("load client failed: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("server shutdown: %v", err)
	}
	cancel()

	leftTun.Close()
	rightTun.Close()

	if err := <-serverErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		log.Printf("server exit: %v", err)
	}
	if err := <-copyErrCh; err != nil && !errors.Is(err, os.ErrClosed) {
		log.Fatalf("tun.Copy failed: %v", err)
	}

	log.Println("load run completed successfully")
}

func startHTTPServer() (*http.Server, <-chan error, error) {
	addr := fmt.Sprintf("%s:%d", rightIP, listenPort)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	chunk := make([]byte, 256*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	totalBytes := int64(*responseMiB) << 20

	mux := http.NewServeMux()
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("serve %s %s", r.RemoteAddr, r.URL.Path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(totalBytes, 10))

		remaining := totalBytes
		for remaining > 0 {
			n := len(chunk)
			if int64(n) > remaining {
				n = int(remaining)
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= int64(n)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{Handler: mux}
	log.Printf("server listening on http://%s/download", addr)
	errCh := make(chan error, 1)
	go func() {
		defer ln.Close()
		errCh <- server.Serve(ln)
	}()
	return server, errCh, nil
}

func runClient() error {
	url := fmt.Sprintf("http://%s:%d/download", rightIP, listenPort)
	totalRequests := *workers * *requestsPerWorker
	wantBytes := int64(totalRequests) * (int64(*responseMiB) << 20)

	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(leftIP)},
		Timeout:   10 * time.Second,
	}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxConnsPerHost:     *workers,
		MaxIdleConns:        *workers,
		MaxIdleConnsPerHost: *workers,
		DisableCompression:  true,
	}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	start := time.Now()
	errCh := make(chan error, *workers)
	bytesCh := make(chan int64, totalRequests)

	for workerID := range *workers {
		go func(workerID int) {
			for reqID := range *requestsPerWorker {
				resp, err := getWithRetry(client, url, 10*time.Second)
				if err != nil {
					errCh <- fmt.Errorf("worker %d request %d: %w", workerID, reqID, err)
					return
				}
				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					errCh <- fmt.Errorf("worker %d request %d: status %s", workerID, reqID, resp.Status)
					return
				}
				n, err := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err != nil {
					errCh <- fmt.Errorf("worker %d request %d: read body: %w", workerID, reqID, err)
					return
				}
				bytesCh <- n
			}
			errCh <- nil
		}(workerID)
	}

	var failed error
	for range *workers {
		if err := <-errCh; err != nil && failed == nil {
			failed = err
		}
	}
	close(bytesCh)

	var totalBytes int64
	for n := range bytesCh {
		totalBytes += n
	}
	if failed != nil {
		return failed
	}
	if totalBytes != wantBytes {
		return fmt.Errorf("unexpected byte count: got=%d want=%d", totalBytes, wantBytes)
	}

	elapsed := time.Since(start)
	mbps := (float64(totalBytes) / (1 << 20)) / elapsed.Seconds()
	log.Printf("client complete: requests=%d bytes=%d elapsed=%s throughput=%.2f MiB/s", totalRequests, totalBytes, elapsed.Round(time.Millisecond), mbps)
	return nil
}

func getWithRetry(client *http.Client, url string, timeout time.Duration) (*http.Response, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return nil, lastErr
}

func waitForServer() error {
	url := fmt.Sprintf("http://%s:%d/healthz", rightIP, listenPort)
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(leftIP)},
		Timeout:   time.Second,
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:        dialer.DialContext,
			DisableCompression: true,
		},
		Timeout: time.Second,
	}
	defer client.CloseIdleConnections()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server did not become ready")
}

func configureInterface(name, addr string, ifaceIdx int, peerIP string) {
	mask := cidrPrefixToMask(prefix)

	runNetsh("interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", name),
		"source=static",
		fmt.Sprintf("addr=%s", addr),
		fmt.Sprintf("mask=%s", mask),
		"gateway=none",
	)
	runNetsh("interface", "set", "interface",
		fmt.Sprintf("name=%s", name),
		"admin=ENABLED",
	)
	runNetsh("interface", "ipv4", "add", "route",
		fmt.Sprintf("%s/32", peerIP),
		fmt.Sprintf("interface=%d", ifaceIdx),
		"store=active",
	)
}

func cleanupInterface(name string, ifaceIdx int, addr, peerIP string) {
	runNetshIgnoreErr("interface", "ipv4", "delete", "route",
		fmt.Sprintf("%s/32", peerIP),
		fmt.Sprintf("interface=%d", ifaceIdx),
		"store=active",
	)
	runNetshIgnoreErr("interface", "ip", "delete", "address",
		fmt.Sprintf("name=%s", name),
		fmt.Sprintf("addr=%s", addr),
	)
}

func isAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	return err == nil || !os.IsPermission(err)
}

func runNetsh(args ...string) {
	log.Printf("running: netsh %v", args)
	cmd := exec.Command("netsh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("netsh %v failed: %v\n%s", args, err, out)
	}
}

func runNetshIgnoreErr(args ...string) {
	cmd := exec.Command("netsh", args...)
	_, _ = cmd.CombinedOutput()
}

func getInterfaceIndex(name string) (int, error) {
	cmd := exec.Command("netsh", "interface", "ipv4", "show", "interfaces")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("list interfaces: %w\n%s", err, out)
	}

	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err == nil {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("interface %q not found", name)
}

func cidrPrefixToMask(prefix int) string {
	mask := uint32(0xFFFFFFFF) << (32 - prefix)
	return fmt.Sprintf("%d.%d.%d.%d",
		(mask>>24)&0xFF,
		(mask>>16)&0xFF,
		(mask>>8)&0xFF,
		mask&0xFF,
	)
}

func logEvents(side string, ch <-chan tun.Event) {
	for event := range ch {
		log.Printf("%s event: %v", side, event)
	}
}
