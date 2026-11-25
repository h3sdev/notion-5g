package main

import "testing"

func TestValidIface(t *testing.T) {
	for _, ok := range []string{"eth0.2", "wlan0.1", "wlan1", "usbnet0", "eth+", "br_x-1"} {
		if !validIface(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "+", "eth0;reboot", "a b", "abcdefghijklmnop", "wlan0/1", "-j"} {
		if validIface(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
