package simplepeer

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

func TestSimplePeer(t *testing.T) {
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
	if _, err = peer2.Write([]byte("World")); err != nil {
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

	if err := peer1.Close(); err != nil {
		t.Fatal(err)
	}

	peer1Closed := <-peer1Close
	peer2Closed := <-peer2Close

	if !peer1Closed || !peer2Closed {
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
		panic(videoTrackErr)
	}

	peer2TrackChan := make(chan *webrtc.TrackRemote)
	peer2.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		peer2TrackChan <- track
	})
	rtpSender, err := peer1.AddTrack(videoTrack)
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

	result := make(chan error)
	done := atomic.Bool{}
	sent := atomic.Int64{}
	go func() {
		file, err := os.Open(testStreamsDataIVFFilename)
		if err != nil {
			done.Store(true)
			result <- err
			return
		}

		ivf, _, err := ivfreader.NewWith(file)
		if err != nil {
			done.Store(true)
			result <- err
			return
		}

		for {
			frame, _, err := ivf.ParseNextFrame()
			if errors.Is(err, io.EOF) {
				done.Store(true)
				result <- nil
				break
			}
			if err != nil {
				done.Store(true)
				result <- err
				return
			}
			if err := videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
				done.Store(true)
				result <- err
				return
			}
			sent.Add(1)
		}
	}()

	trackBuffer := make([]byte, 1500)
	received := &atomic.Int64{}
	rtpPacket := &rtp.Packet{}
	peer2Track := <-peer2TrackChan
	for done.Load() == false {
		read, _, err := peer2Track.Read(trackBuffer)
		if err != nil {
			t.Fatal(err)
		}
		if err = rtpPacket.Unmarshal(trackBuffer[:read]); err != nil {
			t.Fatal(err)
		}
		received.Add(1)
	}

	if received.Load() < sent.Load() {
		t.Fatalf("Received %d packets, but sent %d", received, sent)
	}

	err = <-result
	if err != nil {
		t.Fatal(err)
	}
}
