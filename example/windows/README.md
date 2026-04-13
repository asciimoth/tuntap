# Windows TUN Example (Wintun)

This example demonstrates how to use the `tuntap` library on Windows with the
[Wintun](https://wintun.net/) driver. It creates a native TUN device, configures
it with `netsh`, sets up bidirectional packet forwarding to a virtual TUN server,
and serves HTTP traffic through the tunnel.

## Prerequisites

1. **Go** >= 1.22 installed on Windows

2. **Wintun driver** (`wintun.dll`)
   - Download from https://www.wintun.net/builds/
   - Extract `wintun.dll` and place it in one of these locations:
     - Same directory as the compiled binary
     - A directory in your `PATH`
     - `C:\Windows\System32\` (requires admin)

3. **Administrator privileges**
   - The example must be run from an elevated PowerShell or Command Prompt

## Building

### Option A: Build on Windows

```powershell
# Navigate to the example directory
cd example\windows

# Download dependencies
go mod tidy

# Build the binary
go build -o tuntap-example.exe .
```

## Running

1. **Place `wintun.dll`** next to the binary (or in a directory in your `PATH`).

   ```
   path\to\example\
   ├── tuntap-example.exe
   └── wintun.dll
   ```

2. **Open an elevated PowerShell** (Run as Administrator).

3. **Run the example:**

   ```powershell
   .\tuntap-example.exe
   ```

4. **Test the tunnel** from another terminal (no admin needed):

   ```powershell
   # Ping the virtual server
   ping 10.200.2.1

   # HTTP request (with DNS resolution)
   curl --resolve example.com:80:10.200.2.1 http://example.com

   # Direct HTTP request
   curl http://10.200.2.1
   ```

5. **Stop** with `Ctrl+C`.

