//go:build windows

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
	"strconv"
	"strings"
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

	// IPv4 address and prefix assigned to the native TUN interface (client side).
	nativeTunAddr4   = "10.200.1.2"
	nativeTunPrefix4 = 24

	// Remote IPv4 network (traffic destined here goes into the native TUN).
	remoteNet4       = "10.200.2.0"
	remoteNetPrefix4 = 24

	// Virtual TUN server address (the HTTP server will listen here).
	// This should be in the remote network.
	vtunServerAddr4 = "10.200.2.1"
	vtunServerPort  = 80
)

// isAdmin checks if the current process has administrator privileges.
func isAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	return err == nil || !os.IsPermission(err)
}

// runNetsh runs a netsh command and logs its output.
func runNetsh(args ...string) error {
	log.Printf("Running: netsh %v", args)
	cmd := exec.Command("netsh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh %v failed: %w\noutput: %s", args, err, out)
	}
	return nil
}

func main() {
	if !isAdmin() {
		fmt.Fprintln(os.Stderr, "This example must be run as Administrator (needed for netsh/TUN interface configuration)")
		os.Exit(1)
	}

	log.SetFlags(log.Lmicroseconds | log.Lshortfile)
	log.Println("=== Native TUN (Wintun) + Virtual TUN with HTTP Server ===")

	// ── 1. Create the native TUN device (Wintun) ─────────────────────────
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

	// Check Wintun version
	if nt, ok := nativeTun.(*tuntap.NativeTun); ok {
		version, err := nt.RunningVersion()
		if err == nil {
			log.Printf("Wintun driver version: %d.%d.%d.%d",
				(version>>48)&0xffff,
				(version>>32)&0xffff,
				(version>>16)&0xffff,
				version&0xffff)
		}
	}

	// ── 2. Configure the native interface and routing ────────────────────
	nativeTunAddr4Full := fmt.Sprintf("%s/%d", nativeTunAddr4, nativeTunPrefix4)
	remoteNet4Full := fmt.Sprintf("%s/%d", remoteNet4, remoteNetPrefix4)

	// Get interface index for routing commands
	ifaceIdx, err := getInterfaceIndex(actualName)
	if err != nil {
		log.Fatalf("Failed to get interface index: %v", err)
	}
	log.Printf("Interface index for %s: %d", actualName, ifaceIdx)

	// Configure IP address
	if err := runNetsh("interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", actualName),
		"source=static",
		fmt.Sprintf("addr=%s", nativeTunAddr4),
		fmt.Sprintf("mask=%s", cidrPrefixToMask(nativeTunPrefix4)),
		"gateway=none"); err != nil {
		log.Fatalf("Failed to configure IP address: %v", err)
	}

	// Bring interface up
	if err := runNetsh("interface", "set", "interface",
		fmt.Sprintf("name=%s", actualName),
		"admin=ENABLED"); err != nil {
		log.Fatalf("Failed to enable interface: %v", err)
	}

	// Add route to remote network
	if err := runNetsh("interface", "ipv4", "add", "route",
		remoteNet4Full,
		fmt.Sprintf("interface=%d", ifaceIdx),
		"store=active"); err != nil {
		log.Fatalf("Failed to add route: %v", err)
	}

	log.Printf("Interface %s configured with %s (IPv4)", actualName, nativeTunAddr4Full)
	log.Printf("Route added: %s → interface %d", remoteNet4Full, ifaceIdx)

	// ── 3. Create the virtual TUN (server side) ─────────────────────────
	serverOpts := vtun.Opts{
		LocalAddrs: []netip.Addr{
			netip.MustParseAddr(vtunServerAddr4),
		},
	}
	vtunServer, err := serverOpts.Build()
	if err != nil {
		log.Fatalf("Build virtual TUN server: %v", err)
	}
	defer vtunServer.Close()

	log.Printf("Created virtual TUN server with address %s (IPv4)", vtunServerAddr4)

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
	log.Printf("- curl --resolve example.com:%d:%s  http://example.com (IPv4)", vtunServerPort, vtunServerAddr4)
	log.Printf("- ping %s", vtunServerAddr4)
	log.Println("Press Ctrl+C to exit")

	// ── 7. Wait for shutdown signal ─────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Received shutdown signal")

	// ── 8. Cleanup ──────────────────────────────────────────────────────
	log.Println("Cleaning up routes and interface...")

	cleanupNetshCmds := [][]string{
		{"interface", "ipv4", "delete", "route",
			remoteNet4Full, fmt.Sprintf("interface=%d", ifaceIdx), "store=active"},
		{"interface", "ip", "delete", "address",
			fmt.Sprintf("name=%s", actualName), fmt.Sprintf("addr=%s", nativeTunAddr4)},
	}

	for _, cmd := range cleanupNetshCmds {
		if err := runNetsh(cmd...); err != nil {
			log.Printf("warning: cleanup command %v failed: %v", cmd, err)
		}
	}

	log.Println("Cleanup complete. Exiting.")
}

// getInterfaceIndex retrieves the interface index for a given interface name.
func getInterfaceIndex(name string) (int, error) {
	cmd := exec.Command("netsh", "interface", "ipv4", "show", "interfaces")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to list interfaces: %w\noutput: %s", err, out)
	}

	// Parse output to find interface index
	// Output format:
	// Idx   Met   MTU   State   Name
	//  ---  ----  -----  ------  ------
	//   1     75  4294967295  connected   Loopback Pseudo-Interface 1
	lines := string(out)
	for _, line := range strings.Split(lines, "\n") {
		if strings.Contains(line, name) {
			fields := strings.Fields(line)
			if len(fields) >= 1 {
				idx, err := strconv.Atoi(fields[0])
				if err == nil {
					return idx, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("interface %s not found", name)
}

// cidrPrefixToMask converts a CIDR prefix length to a dotted-decimal subnet mask.
func cidrPrefixToMask(prefix int) string {
	mask := uint32(0xFFFFFFFF) << (32 - prefix)
	return fmt.Sprintf("%d.%d.%d.%d",
		(mask>>24)&0xFF,
		(mask>>16)&0xFF,
		(mask>>8)&0xFF,
		mask&0xFF)
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

	// Handle DNS queries
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := dnsl.ReadFrom(buf)
			if err != nil {
				continue
			}
			resp := handleDNS(buf[:n], netip.MustParseAddr(vtunServerAddr4))
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

	log.Printf("HTTP server listening on 0.0.0.0:%d (IPv4)", vtunServerPort)

	return http.Serve(listener4, nil)
}

// handleDNS creates a minimal DNS response resolving all queries to the server address.
// For A (IPv4) queries, it returns an A record with serverAddr4.
// For other query types, it returns an empty response.
func handleDNS(query []byte, serverAddr4 netip.Addr) []byte {
	if len(query) < 12 {
		return nil
	}

	resp := make([]byte, len(query)+32) // extra space for answer record
	copy(resp, query)

	// Set response flags
	resp[2] = 0x81 // QR=1 (response), OPCODE=0
	resp[3] = 0x80 // RCODE=0 (no error)

	resp[8], resp[9] = 0, 0   // NSCOUNT = 0
	resp[10], resp[11] = 0, 0 // ARCOUNT = 0

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

	// Extract QTYPE
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
		binary.BigEndian.PutUint16(ans[2:4], 1)   // TYPE = A
		binary.BigEndian.PutUint16(ans[4:6], 1)   // CLASS = IN
		binary.BigEndian.PutUint32(ans[6:10], 60) // TTL = 60
		binary.BigEndian.PutUint16(ans[10:12], 4) // RDLENGTH = 4

		addrBytes := serverAddr4.As4()
		ans[12] = addrBytes[0]
		ans[13] = addrBytes[1]
		ans[14] = addrBytes[2]
		ans[15] = addrBytes[3]
		rdLength = 4

	default:
		return resp[:12]
	}

	binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT = 1

	return resp[:ansOffset+12+rdLength]
}
