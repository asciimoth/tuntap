package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/asciimoth/gonnect-netstack/vtun"
	"github.com/asciimoth/gonnect/tun"
	"github.com/asciimoth/tuntap"
)

const (
	// Native TUN interface name and MTU.
	nativeTunName = "tun0"
	nativeTunMTU  = 1500

	// IPv4 address assigned to the native TUN interface (client side).
	nativeTunAddr4 = "10.200.1.2/24"
	// IPv6 address assigned to the native TUN interface (client side).
	nativeTunAddr6 = "fd00:200:1::2/64"

	// Remote IPv4 network (traffic destined here goes into the native TUN).
	remoteNet4 = "10.200.2.0/24"
	// Remote IPv6 network (traffic destined here goes into the native TUN).
	remoteNet6 = "fd00:200:2::/64"

	// Virtual TUN server address (the HTTP server will listen here).
	// These should be in the remote networks.
	vtunServerAddr4 = "10.200.2.1"
	vtunServerAddr6 = "fd00:200:2::1"
	vtunServerPort  = 80
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "This example must be run as root (needs ip/iproute2 privileges)")
		os.Exit(1)
	}

	log.SetFlags(log.Lmicroseconds | log.Lshortfile)
	log.Println("=== Native TUN + Virtual TUN with HTTP Server ===")

	// ── 1. Create the native TUN device ──────────────────────────────────
	nativeTun, err := tuntap.CreateTUN(nativeTunName, nativeTunMTU)
	if err != nil {
		log.Fatalf("CreateTUN: %v", err)
	}
	defer nativeTun.Close()

	actualName, err := nativeTun.Name()
	if err != nil {
		log.Printf("warning: could not get interface name: %v", err)
		actualName = nativeTunName
	}
	log.Printf("Created native TUN device: %s (MTU=%d)", actualName, nativeTunMTU)

	// ── 2. Configure the native interface and routing ────────────────────
	cmds := [][]string{
		{"ip", "link", "set", "dev", actualName, "up"},
		{"ip", "-4", "addr", "add", nativeTunAddr4, "dev", actualName},
		{"ip", "-6", "addr", "add", nativeTunAddr6, "dev", actualName},
		{"ip", "-4", "route", "add", remoteNet4, "dev", actualName},
		{"ip", "-6", "route", "add", remoteNet6, "dev", actualName},
	}

	for _, cmd := range cmds {
		log.Printf("Running: %v", cmd)
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			log.Fatalf("command %v failed: %v\noutput: %s", cmd, err, out)
		}
	}

	log.Printf("Interface %s configured with %s (IPv4) and %s (IPv6)", actualName, nativeTunAddr4, nativeTunAddr6)
	log.Printf("Routes added: %s → %s (IPv4), %s → %s (IPv6)", remoteNet4, actualName, remoteNet6, actualName)

	// ── 3. Create the virtual TUN (server side) ─────────────────────────
	serverOpts := vtun.Opts{
		LocalAddrs: []netip.Addr{
			netip.MustParseAddr(vtunServerAddr4),
			netip.MustParseAddr(vtunServerAddr6),
		},
	}
	vtunServer, err := serverOpts.Build()
	if err != nil {
		log.Fatalf("Build virtual TUN server: %v", err)
	}
	defer vtunServer.Close()

	log.Printf("Created virtual TUN server with addresses %s (IPv4) and %s (IPv6)", vtunServerAddr4, vtunServerAddr6)

	// ── 4. Wait for tunnels to be ready ─────────────────────────────────
	go func() {
		for event := range nativeTun.Events() {
			log.Printf("[native TUN event] %s: %v", actualName, event)
		}
	}()

	<-vtunServer.Events()
	log.Println("Virtual TUN server is ready")

	// ── 5. Start packet forwarding: native TUN ↔ virtual TUN ────────────
	log.Println("Starting bidirectional packet forwarding between native and virtual TUNs...")

	const offset = 16 // room for virtio net header + safety margin

	go func() {
		if err := tun.Copy(nativeTun, vtunServer); err != nil {
			log.Printf("Copy error: %v", err)
		}
	}()

	log.Println("Packet forwarding started")

	// ── 6. Start HTTP server on virtual TUN ─────────────────────────────
	go func() {
		if err := startHTTPServer(vtunServer); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Println("HTTP server started on virtual TUN")
	log.Println("You can now reach the HTTP server via:")
	log.Printf("- curl http://%s:%d (IPv4)", vtunServerAddr4, vtunServerPort)
	log.Printf("- curl http://[%s]:%d (IPv6)", vtunServerAddr6, vtunServerPort)
	log.Printf("- curl --resolve example.com:%d:%s  http://example.com (IPv4)", vtunServerPort, vtunServerAddr4)
	log.Printf("- curl --resolve example.com:%d:[%s]  http://example.com (IPv6)", vtunServerPort, vtunServerAddr6)
	log.Println("Or ping it via:")
	log.Printf("- ping %s", vtunServerAddr4)
	log.Printf("- ping %s", vtunServerAddr6)
	log.Println("Press Ctrl+C to exit")

	// ── 7. Wait for shutdown signal ─────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Received shutdown signal")

	// ── 8. Cleanup ──────────────────────────────────────────────────────
	log.Println("Cleaning up routes and interface...")

	cleanupCmds := [][]string{
		{"ip", "-6", "route", "del", remoteNet6, "dev", actualName},
		{"ip", "-4", "route", "del", remoteNet4, "dev", actualName},
		{"ip", "-6", "addr", "del", nativeTunAddr6, "dev", actualName},
		{"ip", "-4", "addr", "del", nativeTunAddr4, "dev", actualName},
		{"ip", "link", "set", "dev", actualName, "down"},
	}

	for _, cmd := range cleanupCmds {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			log.Printf("warning: cleanup command %v failed: %v\noutput: %s", cmd, err, out)
		}
	}

	log.Println("Cleanup complete. Exiting.")
}

// startHTTPServer sets up and runs an HTTP server on the virtual TUN
func startHTTPServer(vtunServer *vtun.VTun) error {
	// Set up DNS server to resolve all domains to our server addresses
	dnsAddrPort := netip.AddrPortFrom(netip.MustParseAddr(vtunServerAddr4), 53)
	dnsl, err := vtunServer.ListenUDPAddrPort(dnsAddrPort)
	if err != nil {
		return fmt.Errorf("listen DNS: %w", err)
	}

	// Listen for TCP traffic on port 80 (IPv4)
	listener4, err := vtunServer.ListenTCP(
		context.Background(),
		"tcp4",
		fmt.Sprintf("0.0.0.0:%d", vtunServerPort),
	)
	if err != nil {
		return fmt.Errorf("listen TCP4: %w", err)
	}

	// Listen for TCP traffic on port 80 (IPv6)
	listener6, err := vtunServer.ListenTCP(
		context.Background(),
		"tcp6",
		fmt.Sprintf("[%s]:%d", vtunServerAddr6, vtunServerPort),
	)
	if err != nil {
		return fmt.Errorf("listen TCP6: %w", err)
	}

	// Handle DNS queries
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := dnsl.ReadFrom(buf)
			if err != nil {
				continue
			}
			resp := handleDNS(buf[:n], netip.MustParseAddr(vtunServerAddr4), netip.MustParseAddr(vtunServerAddr6))
			_, err = dnsl.WriteTo(resp, addr)
			if err != nil {
				log.Printf("DNS write error: %v", err)
			}
			log.Printf("DNS request served")
		}
	}()

	// HTTP handler
	httpHandler := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("> %s - %s - %s", r.RemoteAddr, r.URL.String(), r.UserAgent())
		io.WriteString(w, "Hello from the virtual TUN HTTP server!\n")
		io.WriteString(w, fmt.Sprintf("Request URL: %s\n", r.URL.String()))
		io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339)))
	}

	http.HandleFunc("/", httpHandler)

	log.Printf("HTTP server listening on 0.0.0.0:%d (IPv4) and [::]:%d (IPv6)", vtunServerPort, vtunServerPort)

	// Serve both IPv4 and IPv6
	errCh := make(chan error, 2)
	go func() { errCh <- http.Serve(listener4, nil) }()
	go func() { errCh <- http.Serve(listener6, nil) }()
	return <-errCh
}

// handleDNS creates a minimal DNS response resolving all queries to server addresses.
// For A (IPv4) queries, it returns an A record with serverAddr4.
// For AAAA (IPv6) queries, it returns an AAAA record with serverAddr6.
// For other query types, it returns an empty response.
func handleDNS(query []byte, serverAddr4, serverAddr6 netip.Addr) []byte {
	if len(query) < 12 {
		return nil
	}

	resp := make([]byte, len(query)+32) // extra space for answer record
	copy(resp, query)

	// Set response flags
	resp[2] = 0x81 // QR=1 (response), OPCODE=0
	resp[3] = 0x80 // RCODE=0 (no error)

	// ANCOUNT = 1 (set below after building answer)
	// QDCOUNT = 1 (already set from query)
	// NSCOUNT = 0
	resp[8], resp[9] = 0, 0
	// ARCOUNT = 0
	resp[10], resp[11] = 0, 0

	// Find the end of the question section
	offset := 12
	for {
		if offset >= len(query) {
			return resp[:12]
		}
		l := int(query[offset])
		offset++
		if l == 0 {
			break
		}
		offset += l
	}
	offset += 4 // Skip QTYPE and QCLASS

	// Extract QTYPE to determine record type
	qtypeOffset := offset - 4
	if qtypeOffset+2 > len(query) {
		return resp[:12]
	}
	qtype := binary.BigEndian.Uint16(query[qtypeOffset : qtypeOffset+2])

	// Build answer record
	ansOffset := offset
	if ansOffset+32 > len(resp) {
		return resp[:12]
	}
	ans := resp[ansOffset:]

	// Name pointer to question
	ans[0] = 0xC0
	ans[1] = 0x0C

	var rdLength int
	switch qtype {
	case 1: // A record (IPv4)
		// TYPE = A
		binary.BigEndian.PutUint16(ans[2:4], 1)
		// CLASS = IN
		binary.BigEndian.PutUint16(ans[4:6], 1)
		// TTL = 60 seconds
		binary.BigEndian.PutUint32(ans[6:10], 60)
		// RDLENGTH = 4 (IPv4 address)
		binary.BigEndian.PutUint16(ans[10:12], 4)

		// RDATA = server address
		addrBytes := serverAddr4.As4()
		ans[12] = addrBytes[0]
		ans[13] = addrBytes[1]
		ans[14] = addrBytes[2]
		ans[15] = addrBytes[3]
		rdLength = 4

	case 28: // AAAA record (IPv6)
		// TYPE = AAAA
		binary.BigEndian.PutUint16(ans[2:4], 28)
		// CLASS = IN
		binary.BigEndian.PutUint16(ans[4:6], 1)
		// TTL = 60 seconds
		binary.BigEndian.PutUint32(ans[6:10], 60)
		// RDLENGTH = 16 (IPv6 address)
		binary.BigEndian.PutUint16(ans[10:12], 16)

		// RDATA = server address
		addrBytes := serverAddr6.As16()
		copy(ans[12:28], addrBytes[:])
		rdLength = 16

	default:
		// Unknown query type, return empty response
		return resp[:12]
	}

	// ANCOUNT = 1
	binary.BigEndian.PutUint16(resp[6:8], 1)

	return resp[:ansOffset+12+rdLength]
}
