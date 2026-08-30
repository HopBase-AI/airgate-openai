package gateway

import "testing"

func TestGuardImageRefAddr(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:80", "[::1]:443", "10.0.0.8:80",
		"192.168.1.1:8080", "169.254.169.254:80", "0.0.0.0:80",
	} {
		if err := guardImageRefAddr(addr); err == nil {
			t.Errorf("%s 应被拒绝", addr)
		}
	}
	if err := guardImageRefAddr("8.8.8.8:443"); err != nil {
		t.Errorf("公网地址不该被拒: %v", err)
	}
}
