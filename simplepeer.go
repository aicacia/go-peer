package simplepeer

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/mitchellh/mapstructure"
	"github.com/pion/webrtc/v4"
)

var (
	errInvalidSignalMessage     = fmt.Errorf("invalid signal message")
	errConnectionNotInitialized = fmt.Errorf("connection not initialized")
)

type SignalMessageType int

const (
	SignalMessageRenegotiate SignalMessageType = iota
	SignalMessageTransceiverRequest
	SignalMessageCandidate
	SignalMessageSdp
)

type SignalMessage struct {
	Type SignalMessageType `json:"type"`
	Data interface{}       `json:"data"`
}

type SignalMessageTransceiver struct {
	Kind webrtc.RTPCodecType         `json:"kind"`
	Init []webrtc.RTPTransceiverInit `json:"init"`
}

type OnSignal func(data SignalMessage) error
type OnConnect func()
type OnData func(message webrtc.DataChannelMessage)
type OnError func(err error)

type PeerOptions struct {
	Id            string
	Initiator     bool
	ChannelName   string
	ChannelConfig *webrtc.DataChannelInit
	Config        *webrtc.Configuration
	OfferConfig   *webrtc.OfferOptions
	AnswerConfig  *webrtc.AnswerOptions
	OnSignal      OnSignal
	OnConnect     OnConnect
	OnData        OnData
	OnError       OnError
}

type Peer struct {
	lock              sync.Mutex
	id                string
	initiator         bool
	channelName       string
	channelConfig     *webrtc.DataChannelInit
	channel           *webrtc.DataChannel
	config            webrtc.Configuration
	connection        *webrtc.PeerConnection
	offerConfig       *webrtc.OfferOptions
	answerConfig      *webrtc.AnswerOptions
	onSignal          OnSignal
	onConnect         []OnConnect
	onData            []OnData
	onError           []OnError
	pendingCandidates []webrtc.ICECandidateInit
}

func NewPeer(options ...PeerOptions) *Peer {
	peer := Peer{
		initiator: false,
		config: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{},
		},
	}
	for _, option := range options {
		if option.Id != "" {
			peer.id = option.Id
		}
		peer.initiator = option.Initiator
		if option.ChannelName != "" {
			peer.channelName = option.ChannelName
		}
		if option.ChannelConfig != nil {
			peer.channelConfig = option.ChannelConfig
		}
		if option.Config != nil {
			peer.config = *option.Config
		}
		if option.AnswerConfig != nil {
			peer.answerConfig = option.AnswerConfig
		}
		if option.OfferConfig != nil {
			peer.offerConfig = option.OfferConfig
		}
		if option.OnSignal != nil {
			peer.onSignal = option.OnSignal
		}
		if option.OnConnect != nil {
			peer.onConnect = append(peer.onConnect, option.OnConnect)
		}
		if option.OnData != nil {
			peer.onData = append(peer.onData, option.OnData)
		}
		if option.OnError != nil {
			peer.onError = append(peer.onError, option.OnError)
		}
	}
	if peer.channelName == "" {
		peer.channelName = uuid.New().String()
	}
	if peer.id == "" {
		peer.id = uuid.New().String()
	}
	return &peer
}

func (peer *Peer) Id() string {
	return peer.id
}

func (peer *Peer) Send(data []byte) error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	return peer.channel.Send(data)
}

func (peer *Peer) Init() error {
	err := peer.createPeer()
	if err != nil {
		return err
	}
	return peer.needsNegotiation()
}

func (peer *Peer) SetOnSignal(fn OnSignal) {
	peer.onSignal = fn
}

func (peer *Peer) AddOnConnect(fn OnConnect) {
	peer.onConnect = append(peer.onConnect, fn)
}

func (peer *Peer) RemoveOnConnect(fn OnConnect) {
	for i, onConnect := range peer.onConnect {
		if &onConnect == &fn {
			peer.onConnect = append(peer.onConnect[:i], peer.onConnect[i+1:]...)
		}
	}
}

func (peer *Peer) AddOnData(fn OnData) {
	peer.onData = append(peer.onData, fn)
}

func (peer *Peer) RemoveOnData(fn OnData) {
	for i, onData := range peer.onData {
		if &onData == &fn {
			peer.onData = append(peer.onData[:i], peer.onData[i+1:]...)
		}
	}
}

func (peer *Peer) createPeer() error {
	err := peer.Close()
	if err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: creating peer", peer.id))
	peer.connection, err = webrtc.NewPeerConnection(peer.config)
	if err != nil {
		return err
	}
	peer.connection.OnConnectionStateChange(peer.onConnectionStateChange)
	peer.connection.OnICECandidate(peer.onICECandidate)
	if peer.initiator {
		peer.channel, err = peer.connection.CreateDataChannel(peer.channelName, peer.channelConfig)
		if err != nil {
			return err
		}
		peer.channel.OnError(peer.onDataChannelError)
		peer.channel.OnOpen(peer.onDataChannelOpen)
		peer.channel.OnMessage(peer.onDataChannelMessage)
	} else {
		peer.connection.OnDataChannel(peer.onDataChannel)
	}
	slog.Debug(fmt.Sprintf("%s: created peer", peer.id))
	return nil
}

func (peer *Peer) Signal(message SignalMessage) error {
	if peer.connection == nil {
		err := peer.createPeer()
		if err != nil {
			return err
		}
	}
	slog.Debug(fmt.Sprintf("%s: received signal type=%v", peer.id, message.Type))
	switch message.Type {
	case SignalMessageRenegotiate:
		return peer.needsNegotiation()
	case SignalMessageTransceiverRequest:
		var transceiver SignalMessageTransceiver
		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			Metadata: nil,
			Result:   &transceiver,
			TagName:  "json",
		})
		if err != nil {
			return err
		}
		if err := decoder.Decode(message.Data); err != nil {
			return err
		}
		_, err = peer.connection.AddTransceiverFromKind(transceiver.Kind, transceiver.Init...)
		return err
	case SignalMessageCandidate:
		var candidate webrtc.ICECandidateInit
		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			Metadata: nil,
			Result:   &candidate,
			TagName:  "json",
		})
		if err != nil {
			return err
		}
		if err := decoder.Decode(message.Data); err != nil {
			return err
		}
		if peer.connection.RemoteDescription() == nil {
			peer.pendingCandidates = append(peer.pendingCandidates, candidate)
			return nil
		} else {
			return peer.connection.AddICECandidate(candidate)
		}
	case SignalMessageSdp:
		var sdp webrtc.SessionDescription
		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			Metadata: nil,
			Result:   &sdp,
			TagName:  "json",
		})
		if err != nil {
			return err
		}
		if err := decoder.Decode(message.Data); err != nil {
			return err
		}
		slog.Debug(fmt.Sprintf("%s: setting remote sdp", peer.id))
		if err := peer.connection.SetRemoteDescription(sdp); err != nil {
			return err
		}
		peer.lock.Lock()
		for _, candidate := range peer.pendingCandidates {
			if err := peer.connection.AddICECandidate(candidate); err != nil {
				slog.Debug(fmt.Sprintf("%s: error adding ice candidate: %s", peer.id, err))
			}
		}
		peer.pendingCandidates = nil
		peer.lock.Unlock()
		if peer.connection.RemoteDescription().Type == webrtc.SDPTypeOffer {
			err = peer.createAnswer()
			if err != nil {
				return err
			}
		}
		return nil
	}
	return errInvalidSignalMessage
}

func (peer *Peer) Close() error {
	var err1, err2 error
	if peer.connection != nil {
		err1 = peer.connection.Close()
		peer.connection = nil
	}
	if peer.channel != nil {
		err2 = peer.channel.Close()
		peer.channel = nil
	}
	return errors.Join(err1, err2)
}

func (peer *Peer) needsNegotiation() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	if peer.initiator {
		return peer.negotiate()
	}
	return nil
}

func (peer *Peer) negotiate() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	if peer.initiator {
		return peer.createOffer()
	} else {
		return peer.onSignal(SignalMessage{
			Type: SignalMessageRenegotiate,
		})
	}
}

func (peer *Peer) createOffer() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	slog.Debug(fmt.Sprintf("%s: creating offer", peer.id))
	offer, err := peer.connection.CreateOffer(peer.offerConfig)
	if err != nil {
		return err
	}
	if err := peer.connection.SetLocalDescription(offer); err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: created offer", peer.id))
	return peer.onSignal(SignalMessage{
		Type: SignalMessageSdp,
		Data: toJSON(offer),
	})
}

func (peer *Peer) createAnswer() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	slog.Debug(fmt.Sprintf("%s: creating answer", peer.id))
	answer, err := peer.connection.CreateAnswer(peer.answerConfig)
	if err != nil {
		return err
	}
	if err := peer.connection.SetLocalDescription(answer); err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: created answer", peer.id))
	return peer.onSignal(SignalMessage{
		Type: SignalMessageSdp,
		Data: toJSON(answer),
	})
}

func (peer *Peer) connect() {
	for _, fn := range peer.onConnect {
		fn()
	}
}

func (peer *Peer) error(err error) {
	for _, fn := range peer.onError {
		fn(err)
	}
}

func (peer *Peer) onDataChannelError(err error) {
	peer.error(err)
}

func (peer *Peer) onDataChannelOpen() {
	peer.connect()
}

// onDataChannelMessage is called when a message is received from the data channel

func (peer *Peer) onDataChannelMessage(message webrtc.DataChannelMessage) {
	for _, fn := range peer.onData {
		fn(message)
	}
}

func (peer *Peer) onConnectionStateChange(pcs webrtc.PeerConnectionState) {
	switch pcs {
	case webrtc.PeerConnectionStateUnknown:
		slog.Debug(fmt.Sprintf("%s: connection state unknown", peer.id))
	case webrtc.PeerConnectionStateNew:
		slog.Debug(fmt.Sprintf("%s: connection new", peer.id))
	case webrtc.PeerConnectionStateConnecting:
		slog.Debug(fmt.Sprintf("%s: connecting", peer.id))
	case webrtc.PeerConnectionStateConnected:
		slog.Debug(fmt.Sprintf("%s: connection established", peer.id))
	case webrtc.PeerConnectionStateDisconnected:
		slog.Debug(fmt.Sprintf("%s: connection disconnected", peer.id))
		peer.Close()
	case webrtc.PeerConnectionStateFailed:
		slog.Debug(fmt.Sprintf("%s: connection failed", peer.id))
		peer.Close()
	case webrtc.PeerConnectionStateClosed:
		slog.Debug(fmt.Sprintf("%s: connection closed", peer.id))
		peer.Close()
	}
}

func (peer *Peer) onICECandidate(pendingCandidate *webrtc.ICECandidate) {
	if pendingCandidate == nil {
		return
	}
	if peer.connection.RemoteDescription() == nil {
		peer.lock.Lock()
		defer peer.lock.Unlock()
		peer.pendingCandidates = append(peer.pendingCandidates, pendingCandidate.ToJSON())
	} else {
		iceCandidateInit := pendingCandidate.ToJSON()
		err := peer.onSignal(SignalMessage{
			Type: SignalMessageCandidate,
			Data: toJSON(iceCandidateInit),
		})
		if err != nil {
			slog.Debug(fmt.Sprintf("%s: error sending ice candidate: %s", peer.id, err))
		}
	}
}

func (peer *Peer) onDataChannel(dc *webrtc.DataChannel) {
	peer.channel = dc
	peer.channel.OnError(peer.onDataChannelError)
	peer.channel.OnOpen(peer.onDataChannelOpen)
	peer.channel.OnMessage(peer.onDataChannelMessage)
}
