//go:build linux

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
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/asciimoth/gonnect/tun"
	"github.com/asciimoth/tuntap"
)

const (
	leftTunName  = "tunload0"
	rightTunName = "tunload1"

	leftNS  = "tunload-ns-a"
	rightNS = "tunload-ns-b"

	leftAddr  = "10.231.0.1/24"
	rightAddr = "10.231.0.2/24"

	leftIP  = "10.231.0.1"
	rightIP = "10.231.0.2"

	listenPort = 18080
	tunMTU     = 1500
)

var (
	workers           = flag.Int("workers", 8, "number of parallel download workers")
	requestsPerWorker = flag.Int("requests-per-worker", 4, "requests per worker")
	responseMiB       = flag.Int("response-mib", 16, "response size per request in MiB")
	mode              = flag.String("mode", "main", "internal mode: main, serve, client")
)

func main() {
	flag.Parse()

	log.SetFlags(log.Lmicroseconds | log.Lshortfile)

	switch *mode {
	case "serve":
		runServer()
	case "client":
		runClient()
	default:
		runMain()
	}
}

func runMain() {
	if os.Geteuid() != 0 {
		log.Fatal("this example must be run as root")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cleanupNamespaces()
	defer cleanupNamespaces()

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

	mustRun("ip", "netns", "add", leftNS)
	mustRun("ip", "netns", "add", rightNS)
	mustRun("ip", "link", "set", "dev", leftActual, "netns", leftNS)
	mustRun("ip", "link", "set", "dev", rightActual, "netns", rightNS)

	configureNamespace(leftNS, leftActual, leftAddr)
	configureNamespace(rightNS, rightActual, rightAddr)

	go logEvents("left", leftTun.Events())
	go logEvents("right", rightTun.Events())

	copyErrCh := make(chan error, 1)
	go func() {
		copyErrCh <- tun.Copy(leftTun, rightTun)
	}()

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("resolve executable: %v", err)
	}

	serverCmd := exec.CommandContext(
		ctx,
		"ip", "netns", "exec", rightNS, exe,
		"-mode=serve",
		"-response-mib="+strconv.Itoa(*responseMiB),
	)
	serverCmd.Stdout = os.Stdout
	serverCmd.Stderr = os.Stderr
	if err := serverCmd.Start(); err != nil {
		log.Fatalf("start server: %v", err)
	}

	clientCmd := exec.CommandContext(
		ctx,
		"ip", "netns", "exec", leftNS, exe,
		"-mode=client",
		"-workers="+strconv.Itoa(*workers),
		"-requests-per-worker="+strconv.Itoa(*requestsPerWorker),
		"-response-mib="+strconv.Itoa(*responseMiB),
	)
	clientCmd.Stdout = os.Stdout
	clientCmd.Stderr = os.Stderr

	log.Printf("starting load: workers=%d requests-per-worker=%d response-mib=%d", *workers, *requestsPerWorker, *responseMiB)
	if err := clientCmd.Run(); err != nil {
		_ = serverCmd.Process.Kill()
		log.Fatalf("load client failed: %v", err)
	}

	stop()
	waitProcess(serverCmd)
	leftTun.Close()
	rightTun.Close()

	if err := <-copyErrCh; err != nil && !errors.Is(err, os.ErrClosed) {
		log.Fatalf("tun.Copy failed: %v", err)
	}

	log.Println("load run completed successfully")
}

func runServer() {
	addr := fmt.Sprintf("%s:%d", rightIP, listenPort)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	defer ln.Close()

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
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

func runClient() {
	url := fmt.Sprintf("http://%s:%d/download", rightIP, listenPort)
	totalRequests := *workers * *requestsPerWorker
	wantBytes := int64(totalRequests) * (int64(*responseMiB) << 20)

	transport := &http.Transport{
		MaxConnsPerHost:     *workers,
		MaxIdleConns:        *workers,
		MaxIdleConnsPerHost: *workers,
		DisableCompression:  true,
	}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	start := time.Now()
	errCh := make(chan error, totalRequests)
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

	var (
		totalBytes int64
		failed     error
	)
	for range *workers {
		if err := <-errCh; err != nil && failed == nil {
			failed = err
		}
	}
	close(bytesCh)

	for n := range bytesCh {
		totalBytes += n
	}
	if failed != nil {
		log.Fatal(failed)
	}
	if totalBytes != wantBytes {
		log.Fatalf("unexpected byte count: got=%d want=%d", totalBytes, wantBytes)
	}

	elapsed := time.Since(start)
	mbps := (float64(totalBytes) / (1 << 20)) / elapsed.Seconds()
	log.Printf("client complete: requests=%d bytes=%d elapsed=%s throughput=%.2f MiB/s", totalRequests, totalBytes, elapsed.Round(time.Millisecond), mbps)
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

func configureNamespace(namespace, ifName, addr string) {
	mustRun("ip", "netns", "exec", namespace, "ip", "link", "set", "dev", "lo", "up")
	mustRun("ip", "netns", "exec", namespace, "ip", "addr", "add", addr, "dev", ifName)
	mustRun("ip", "netns", "exec", namespace, "ip", "link", "set", "dev", ifName, "up")
}

func waitProcess(cmd *exec.Cmd) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("process exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			<-done
		}
	}
}

func cleanupNamespaces() {
	runIgnoringError("ip", "netns", "del", leftNS)
	runIgnoringError("ip", "netns", "del", rightNS)
}

func mustRun(name string, args ...string) {
	log.Printf("running: %s %v", name, args)
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

func runIgnoringError(name string, args ...string) {
	cmd := exec.Command(name, args...)
	_, _ = cmd.CombinedOutput()
}

func logEvents(side string, ch <-chan tun.Event) {
	for event := range ch {
		log.Printf("%s event: %v", side, event)
	}
}
