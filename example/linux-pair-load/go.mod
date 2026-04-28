module github.com/asciimoth/tuntap/example/linux-pair-load

go 1.25.5

require (
	github.com/asciimoth/gonnect v0.11.0
	github.com/asciimoth/tuntap v0.1.14
)

require (
	github.com/asciimoth/bufpool v0.3.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
)

replace github.com/asciimoth/tuntap => ../..
