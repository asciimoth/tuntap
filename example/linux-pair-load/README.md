# Linux Native TUN Pair Load Example

This example creates two native Linux TUN devices, moves them into two temporary
network namespaces, connects them with `github.com/asciimoth/gonnect/tun.Copy`,
and drives heavy parallel HTTP downloads through the pair.

It is intended as a practical stress harness for reproducing TUN copy-path bugs
under load on a single machine.

## Prerequisites

- Linux with `iproute2`
- Root privileges
- Go 1.25+

## Run

```sh
cd example/linux-pair-load
go build ./
sudo ./linux-pair-load
```

The program will:

1. Create `tunload0` and `tunload1`
2. Move them into temporary namespaces
3. Start `tun.Copy(tunA, tunB)`
4. Start an HTTP server in one namespace
5. Start a parallel download client in the other namespace
6. Print total transferred bytes and throughput

Defaults are intentionally heavy enough to move a large amount of traffic, but
you can tune them with flags:

```sh
go run . -workers 12 -requests-per-worker 6 -response-mib 32
```
