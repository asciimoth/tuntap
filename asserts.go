package tuntap

import (
	// "github.com/asciimoth/gonnect"
	"github.com/asciimoth/gonnect/tun"
)

// Static type assertions
var (
	// _ gonnect.UpDown           = &NativeTun{} // TODO: Implement UpDown
	_ tun.Tun                  = &NativeTun{}
)
