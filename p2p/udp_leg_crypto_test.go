package p2p

import (
	"bytes"
	"testing"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/util"
)

func TestUDPLegCryptoRoundTrip(t *testing.T) {
	tr := &udpLegTransport{}
	tr.setLegCrypto("proxy-pass")
	dgram := frame.EncodeDatagram(frame.FrameMeshPacket, []byte("hello-leg"))
	sealed := tr.cryptoSeal(dgram)
	if bytes.Equal(sealed, dgram) {
		t.Fatal("datagram was not encrypted")
	}
	if len(sealed) != len(dgram)+40 {
		t.Fatalf("sealed len = %d, want %d", len(sealed), len(dgram)+40)
	}
	plain, ok := tr.cryptoOpen(sealed)
	if !ok || !bytes.Equal(plain, dgram) {
		t.Fatalf("round trip failed: ok=%v plain=%x", ok, plain)
	}
}

func TestUDPLegCryptoEmptySecretPassthrough(t *testing.T) {
	tr := &udpLegTransport{}
	tr.setLegCrypto("")
	dgram := frame.EncodeDatagram(frame.FrameLinkPing, []byte("ping"))
	if got := tr.cryptoSeal(dgram); !bytes.Equal(got, dgram) {
		t.Fatal("empty secret must not change the datagram")
	}
	plain, ok := tr.cryptoOpen(dgram)
	if !ok || !bytes.Equal(plain, dgram) {
		t.Fatal("empty secret must pass the datagram through")
	}
}

func TestUDPLegCryptoWrongSecretDrops(t *testing.T) {
	sender := &udpLegTransport{}
	sender.setLegCrypto("pass-a")
	sealed := sender.cryptoSeal(frame.EncodeDatagram(frame.FrameMeshPacket, []byte("x")))

	receiver := &udpLegTransport{}
	receiver.setLegCrypto("pass-b")
	if _, ok := receiver.cryptoOpen(sealed); ok {
		t.Fatal("wrong secret must fail authentication")
	}
}

// TestP2PLegSecretPerProxy pins the end-to-end key contract: the passive side
// holds the credential the carrier authenticates with, so the active side must
// derive the same value from its proxy config.
func TestP2PLegSecretPerProxy(t *testing.T) {
	cases := []struct {
		name string
		p    *config.Proxy
		want string
	}{
		{"nil proxy", nil, ""},
		{"h_tunnel verbatim", &config.Proxy{Type: config.ProxyH_TUNNEL, Password: "pw"}, "pw"},
		{"socks5 verbatim", &config.Proxy{Type: config.ProxySOCKS5, Password: "pw"}, "pw"},
		{"socks5 no password", &config.Proxy{Type: config.ProxySOCKS5}, ""},
		{"trojan hashed", &config.Proxy{Type: config.ProxyTROJAN, Password: "pw"}, util.Sha224Hex("pw")},
	}
	for _, c := range cases {
		if got := p2pLegSecret(c.p); got != c.want {
			t.Fatalf("%s: p2pLegSecret = %q, want %q", c.name, got, c.want)
		}
	}
}
