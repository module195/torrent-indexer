package goscrape

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

// A connect answer carrying another request's transaction id must be
// rejected (the check used to compare the id with itself).
func TestConnectRejectsWrongTransactionID(t *testing.T) {
	allowInternalTrackers(t)
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, err := conn.ReadFrom(buf)
		if err != nil || n < 16 {
			return
		}
		out := make([]byte, 16)
		binary.BigEndian.PutUint32(out[0:], actionConnect)
		binary.BigEndian.PutUint32(out[4:], binary.BigEndian.Uint32(buf[12:])+1)
		binary.BigEndian.PutUint64(out[8:], 42)
		_, _ = conn.WriteTo(out, addr)
	}()

	g, err := New("udp://" + conn.LocalAddr().String() + "/announce")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.SetTimeout(time.Second)
	g.SetRetryLimit(0)

	if _, _, err := g.connect(); !errors.Is(err, ErrInvalidTransactionID) {
		t.Fatalf("want ErrInvalidTransactionID, got %v", err)
	}
}
