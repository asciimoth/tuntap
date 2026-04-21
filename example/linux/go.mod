module github.com/asciimoth/tuntap/example/linux

go 1.25.5

require (
	github.com/asciimoth/gonnect v0.5.0
	github.com/asciimoth/gonnect-netstack v0.3.5
	github.com/asciimoth/tuntap v0.1.3
)

require (
	github.com/asciimoth/bufpool v0.3.0 // indirect
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/exp v0.0.0-20231110203233-9a3e6036ecaa // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	gvisor.dev/gvisor v0.0.0-20260325202830-7644cf3a343c // indirect
)

replace github.com/asciimoth/tuntap => ../..
