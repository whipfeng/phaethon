package p2p

import (
	"bytes"
	"testing"

	"phaethon/frame"
)

func TestUDPLegCryptoRoundTrip(t *testing.T) {
	tr := &udpLegTransport{}
	tr.setLegCrypto("mesh-psk")
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

func TestUDPLegCryptoEmptyKeyPassthrough(t *testing.T) {
	tr := &udpLegTransport{}
	tr.setLegCrypto("")
	dgram := frame.EncodeDatagram(frame.FrameLinkPing, []byte("ping"))
	if got := tr.cryptoSeal(dgram); !bytes.Equal(got, dgram) {
		t.Fatal("empty key must not change the datagram")
	}
	plain, ok := tr.cryptoOpen(dgram)
	if !ok || !bytes.Equal(plain, dgram) {
		t.Fatal("empty key must pass the datagram through")
	}
}

func TestUDPLegCryptoWrongKeyDrops(t *testing.T) {
	sender := &udpLegTransport{}
	sender.setLegCrypto("key-a")
	sealed := sender.cryptoSeal(frame.EncodeDatagram(frame.FrameMeshPacket, []byte("x")))

	receiver := &udpLegTransport{}
	receiver.setLegCrypto("key-b")
	if _, ok := receiver.cryptoOpen(sealed); ok {
		t.Fatal("wrong key must fail authentication")
	}
}
