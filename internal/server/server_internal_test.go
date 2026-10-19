package server

import "testing"

func TestIsLoopback(t *testing.T) {
	loopback := []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"}
	for _, addr := range loopback {
		if !isLoopback(addr) {
			t.Fatalf("%s should be loopback", addr)
		}
	}
	nonLoopback := []string{":8787", "0.0.0.0:8787", "[::]:8787", "192.168.1.10:8787"}
	for _, addr := range nonLoopback {
		if isLoopback(addr) {
			t.Fatalf("%s should not be loopback", addr)
		}
	}
}
