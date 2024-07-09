package peer

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

func TestPeer(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	peer1Connect := make(chan bool)
	peer2Connect := make(chan bool)
	peer1Close := make(chan bool)
	peer2Close := make(chan bool)
	peer1Data := make(chan []byte)
	peer2Data := make(chan []byte)

	var peer1, peer2 *Peer
	peer1 = NewPeer(PeerOptions{
		Id: "peer1",
		OnSignal: func(message map[string]interface{}) error {
			return peer2.Signal(message)
		},
		OnConnect: func() {
			peer1Connect <- true
		},
		OnData: func(message webrtc.DataChannelMessage) {
			peer1Data <- message.Data
		},
		OnClose: func() {
			peer1Close <- true
		},
	})
	peer2 = NewPeer(PeerOptions{
		Id: "peer2",
		OnSignal: func(message map[string]interface{}) error {
			return peer1.Signal(message)
		},
		OnConnect: func() {
			peer2Connect <- true
		},
		OnData: func(message webrtc.DataChannelMessage) {
			peer2Data <- message.Data
		},
		OnClose: func() {
			peer2Close <- true
		},
	})
	err := peer1.Init()
	if err != nil {
		t.Fatal(err)
	}

	peer1Connected := <-peer1Connect
	peer2Connected := <-peer2Connect

	if !peer1Connected || !peer2Connected {
		t.Fatal("peers did not connect")
	}

	peer2Reader := peer2.Reader()
	if _, err = peer1.Write([]byte("Hello")); err != nil {
		t.Fatal(err)
	}
	peer2Bytes := make([]byte, 5)
	if _, err = peer2Reader.Read(peer2Bytes); err != nil {
		t.Fatal(err)
	} else if string(peer2Bytes) != "Hello" {
		t.Fatalf("expected 'Hello', got '%s'", string(peer2Bytes))
	}
	if err := peer2Reader.Close(); err != nil {
		t.Fatal(err)
	}
	peer2DataBytes := <-peer2Data
	if string(peer2DataBytes) != "Hello" {
		t.Fatalf("expected 'Hello', got '%s'", string(peer2DataBytes))
	}

	peer1Reader := peer1.Reader()
	if _, err = peer2.WriteText("World"); err != nil {
		t.Fatal(err)
	}
	peer1Bytes := make([]byte, 5)
	if _, err = peer1Reader.Read(peer1Bytes); err != nil {
		t.Fatal(err)
	} else if string(peer1Bytes) != "World" {
		t.Fatalf("expected 'World', got '%s'", string(peer1Bytes))
	}
	if err := peer1Reader.Close(); err != nil {
		t.Fatal(err)
	}
	peer1DataBytes := <-peer1Data
	if string(peer1DataBytes) != "World" {
		t.Fatalf("expected 'World', got '%s'", string(peer1DataBytes))
	}

	peer2CloseSignal := peer2.CloseSignal()
	if err := peer1.Close(); err != nil {
		t.Fatal(err)
	}

	peer1Closed := <-peer1Close
	peer2Closed := <-peer2Close
	peer2CloseReceived := <-peer2CloseSignal

	if !peer1Closed || !peer2Closed || !peer2CloseReceived {
		t.Fatal("peers did not close")
	}
}

const testStreamsDataIVFFilename = "file_example_MP4_480_1_5MG.ivf"

func TestStreams(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	peer1Connect := make(chan bool)
	peer2Connect := make(chan bool)

	var peer1, peer2 *Peer
	peer1 = NewPeer(PeerOptions{
		Id: "peer1",
		OnSignal: func(message map[string]interface{}) error {
			return peer2.Signal(message)
		},
		OnConnect: func() {
			peer1Connect <- true
		},
	})
	peer2 = NewPeer(PeerOptions{
		Id: "peer2",
		OnSignal: func(message map[string]interface{}) error {
			return peer1.Signal(message)
		},
		OnConnect: func() {
			peer2Connect <- true
		},
	})
	err := peer1.Init()
	if err != nil {
		t.Fatal(err)
	}

	<-peer1Connect
	<-peer2Connect

	file, err := os.Open(testStreamsDataIVFFilename)
	if err != nil {
		t.Fatal(err)
	}

	_, header, err := ivfreader.NewWith(file)
	if err != nil {
		t.Fatal(err)
	}

	var trackCodec string
	switch header.FourCC {
	case "AV01":
		trackCodec = webrtc.MimeTypeAV1
	case "VP90":
		trackCodec = webrtc.MimeTypeVP9
	case "VP80":
		trackCodec = webrtc.MimeTypeVP8
	default:
		t.Fatalf("Unable to handle FourCC %s", header.FourCC)
	}

	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: trackCodec}, "video", "pion")
	if videoTrackErr != nil {
		t.Fatal(err)
	}

	peer2TrackChan := make(chan *webrtc.TrackRemote)
	peer2.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		peer2TrackChan <- track
	})
	rtpSender, err := peer1.AddTrack(videoTrack)
	if err != nil {
		t.Fatal(err)
	}
	_, err = peer2.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	sent := atomic.Int64{}
	go func() {
		defer peer1.RemoveTrack(rtpSender)
		file, err := os.Open(testStreamsDataIVFFilename)
		if err != nil {
			slog.Error("ivfreader", "err", err)
			return
		}

		ivf, _, err := ivfreader.NewWith(file)
		if err != nil {
			slog.Error("ivfreader", "err", err)
			return
		}

		for {
			frame, _, err := ivf.ParseNextFrame()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				slog.Error("ivfreader", "err", err)
				return
			}
			if err := videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
				slog.Error("ivfreader", "err", err)
				return
			}
			sent.Add(1)
		}
	}()

	var peer2Track *webrtc.TrackRemote
	select {
	case peer2Track = <-peer2TrackChan:
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for track")
	}

	received := atomic.Int64{}
	for {
		_, _, err := peer2Track.ReadRTP()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		received.Add(1)
	}

	if received.Load() == 0 || sent.Load() == 0 {
		t.Fatalf("Received %d packets, but sent %d", received.Load(), sent.Load())
	}
}
