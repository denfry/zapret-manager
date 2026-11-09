package main

import "testing"

func TestParseTCPTimestamps(t *testing.T) {
	cases := map[string]string{
		"Receive-Side Scaling State          : enabled\r\nRFC 1323 Timestamps                 : disabled\r\n": "disabled",
		"RFC 1323 Timestamps                 : enabled\r\n":                                                   "enabled",
		"\x8c\xa5\xe2\xaa\xa8 RFC 1323 : enabled\r\n":                                                         "enabled",
		"RFC 1323 Timestamps : allowed\r\n":                                                                   "",
		"":                                                                                                    "",
	}
	for in, want := range cases {
		if got := parseTCPTimestamps(in); got != want {
			t.Errorf("parseTCPTimestamps(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTunnelAdapter(t *testing.T) {
	yes := []string{"tun0", "wintun", "WireGuard Tunnel", "AmneziaWG", "sing-box", "TAP-Windows Adapter V9", "Happ", "ProtonVPN", "Outline-TAP", "Clash"}
	no := []string{"Teredo Tunneling Pseudo-Interface", "Ethernet", "Wi-Fi", "Singularity", "Loopback Pseudo-Interface 1", "isatap.{1234}"}
	for _, n := range yes {
		if !isTunnelAdapter(n) {
			t.Errorf("%q должен считаться туннелем", n)
		}
	}
	for _, n := range no {
		if isTunnelAdapter(n) {
			t.Errorf("%q не туннель", n)
		}
	}
}

func TestHostsHasYouTube(t *testing.T) {
	if hostsHasYouTube("# 1.2.3.4 youtube.com\r\n127.0.0.1 localhost # youtube.com\r\n") {
		t.Error("комментарии — не записи")
	}
	if hostsHasYouTube("1.2.3.4 notyoutube.com\r\n") {
		t.Error("чужой домен")
	}
	for _, s := range []string{"0.0.0.0 www.youtube.com", "1.2.3.4\tyoutu.be", "1.2.3.4 a.b YOUTUBE.COM."} {
		if !hostsHasYouTube(s) {
			t.Errorf("%q — запись для YouTube", s)
		}
	}
}
