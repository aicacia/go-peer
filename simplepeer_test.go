package simplepeer

import (
	"log/slog"
	"os"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestSimplePeer(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	peer1Connect := make(chan bool)
	peer2Connect := make(chan bool)
	peer1Data := make(chan []byte)
	peer2Data := make(chan []byte)

	var peer1, peer2 *Peer
	peer1 = NewPeer(PeerOptions{
		Id:        "peer1",
		Initiator: true,
		OnSignal: func(data SignalMessage) error {
			return peer2.Signal(data)
		},
		OnConnect: func() {
			peer1Connect <- true
		},
		OnData: func(message webrtc.DataChannelMessage) {
			peer1Data <- message.Data
		},
	})
	defer peer1.Close()
	peer2 = NewPeer(PeerOptions{
		Id:        "peer2",
		Initiator: false,
		OnSignal: func(data SignalMessage) error {
			return peer1.Signal(data)
		},
		OnConnect: func() {
			peer2Connect <- true
		},
		OnData: func(message webrtc.DataChannelMessage) {
			peer2Data <- message.Data
		},
	})
	defer peer2.Close()
	err := peer1.Init()
	if err != nil {
		t.Fatal(err)
	}

	peer1Connected := <-peer1Connect
	peer2Connected := <-peer2Connect

	if !peer1Connected || !peer2Connected {
		t.Fatal("peer did not connect")
	}

	err = peer1.Send([]byte("Hello"))
	if err != nil {
		t.Fatal(err)
	}
	peer2DataReceived := <-peer2Data
	if string(peer2DataReceived) != "Hello" {
		t.Fatalf("expected 'Hello', got '%s'", string(peer2DataReceived))
	}
	err = peer2.Send([]byte("World"))
	if err != nil {
		t.Fatal(err)
	}
	peer1DataReceived := <-peer1Data
	if string(peer1DataReceived) != "World" {
		t.Fatalf("expected 'World', got '%s'", string(peer1DataReceived))
	}
}
