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
	sent := atomic.Int64{}
	go func() {
		file, err := os.Open(testStreamsDataIVFFilename)
		if err != nil {
			result <- err
			return
		}

		ivf, _, err := ivfreader.NewWith(file)
		if err != nil {
			result <- err
			return
		}

		for {
			frame, _, err := ivf.ParseNextFrame()
			if errors.Is(err, io.EOF) {
				result <- nil
				break
			}
			if err != nil {
				result <- err
				return
			}
			if err := videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
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
	for received.Load() < sent.Load() {
		count, _, err := peer2Track.Read(trackBuffer)
		if err != nil {
			t.Fatal(err)
		}
		if err = rtpPacket.Unmarshal(trackBuffer[:count]); err != nil {
			t.Fatal(err)
		}
		received.Add(1)
	}

	err = <-result
	if err != nil {
		t.Fatal(err)
	}
}
