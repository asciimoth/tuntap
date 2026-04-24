# Windows Native TUN Pair Load Example

This example creates two Wintun-backed native TUN devices, pairs them with
`github.com/asciimoth/gonnect/tun.Copy`, and drives parallel HTTP downloads
between two addresses bound to the pair.

It is intended as a practical stress harness for the TUN copy path on Windows.

## Prerequisites

- Windows
- Administrator privileges
- `wintun.dll` available next to the binary or in `PATH`
- Go 1.25+

## Run

```powershell
cd example\windows-pair-load
go run .
```

You can make the load heavier with flags:

```powershell
go run . -workers 12 -requests-per-worker 6 -response-mib 32
```

The client binds its source socket to the left adapter address so traffic is
forced onto the TUN route from that side.
