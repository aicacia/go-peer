package simplepeer

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

var (
	errInvalidSignalMessageType = fmt.Errorf("invalid signal message type")
	errInvalidSignalMessage     = fmt.Errorf("invalid signal message")
	errConnectionNotInitialized = fmt.Errorf("connection not initialized")
)

const (
	SignalMessageRenegotiate        = "renegotiate"
	SignalMessageTransceiverRequest = "transceiver_request"
	SignalMessageCandidate          = "candidate"
	SignalMessageAnswer             = "answer"
	SignalMessageOffer              = "offer"
	SignalMessagePRAnswer           = "pranswer"
	SignalMessageRollback           = "rollback"
)

type SignalMessage = map[string]interface{}

type SignalMessageTransceiver struct {
	Kind webrtc.RTPCodecType         `json:"kind"`
	Init []webrtc.RTPTransceiverInit `json:"init"`
}

type OnSignal func(message SignalMessage) error
type OnConnect func()
type OnData func(message webrtc.DataChannelMessage)
type OnError func(err error)
type OnClose func()

type PeerOptions struct {
	Id            string
	ChannelName   string
	ChannelConfig *webrtc.DataChannelInit
	Config        *webrtc.Configuration
	OfferConfig   *webrtc.OfferOptions
	AnswerConfig  *webrtc.AnswerOptions
	OnSignal      OnSignal
	OnConnect     OnConnect
	OnData        OnData
	OnError       OnError
	OnClose       OnClose
}

type Peer struct {
	id                    string
	initiator             bool
	channelName           string
	channelConfig         *webrtc.DataChannelInit
	channel               *webrtc.DataChannel
	config                webrtc.Configuration
	connection            *webrtc.PeerConnection
	offerConfig           *webrtc.OfferOptions
	answerConfig          *webrtc.AnswerOptions
	onSignal              OnSignal
	onConnect             []OnConnect
	onData                []OnData
	onError               []OnError
	onClose               []OnClose
	pendingCandidatesLock sync.Mutex
	pendingCandidates     []webrtc.ICECandidateInit
}

func NewPeer(options ...PeerOptions) *Peer {
	peer := Peer{
		config: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{},
		},
	}
	for _, option := range options {
		if option.Id != "" {
			peer.id = option.Id
		}
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
		if option.OnClose != nil {
			peer.onClose = append(peer.onClose, option.OnClose)
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

func (peer *Peer) Connection() *webrtc.PeerConnection {
	return peer.connection
}

func (peer *Peer) Channel() *webrtc.DataChannel {
	return peer.channel
}

func (peer *Peer) Send(data []byte) error {
	if peer.channel == nil {
		return errConnectionNotInitialized
	}
	return peer.channel.Send(data)
}

func (peer *Peer) Init() error {
	peer.initiator = true
	err := peer.createPeer()
	if err != nil {
		return err
	}
	return peer.needsNegotiation()
}

func (peer *Peer) OnSignal(fn OnSignal) {
	peer.onSignal = fn
}

func (peer *Peer) OnConnect(fn OnConnect) {
	peer.onConnect = append(peer.onConnect, fn)
}

func (peer *Peer) OffConnect(fn OnConnect) {
	for i, onConnect := range peer.onConnect {
		if &onConnect == &fn {
			peer.onConnect = append(peer.onConnect[:i], peer.onConnect[i+1:]...)
		}
	}
}

func (peer *Peer) OnData(fn OnData) {
	peer.onData = append(peer.onData, fn)
}

func (peer *Peer) OffData(fn OnData) {
	for i, onData := range peer.onData {
		if &onData == &fn {
			peer.onData = append(peer.onData[:i], peer.onData[i+1:]...)
		}
	}
}

func (peer *Peer) OnError(fn OnError) {
	peer.onError = append(peer.onError, fn)
}

func (peer *Peer) OffError(fn OnError) {
	for i, onError := range peer.onError {
		if &onError == &fn {
			peer.onError = append(peer.onError[:i], peer.onError[i+1:]...)
		}
	}
}

func (peer *Peer) OnClose(fn OnClose) {
	peer.onClose = append(peer.onClose, fn)
}

func (peer *Peer) OffClose(fn OnClose) {
	for i, onClose := range peer.onClose {
		if &onClose == &fn {
			peer.onClose = append(peer.onClose[:i], peer.onClose[i+1:]...)
		}
	}
}

func (peer *Peer) Signal(message SignalMessage) error {
	if peer.connection == nil {
		err := peer.createPeer()
		if err != nil {
			return err
		}
	}
	messageType, ok := message["type"].(string)
	if !ok {
		return errInvalidSignalMessageType
	}
	slog.Debug(fmt.Sprintf("%s: received signal message=%v", peer.id, message))
	switch messageType {
	case SignalMessageRenegotiate:
		return peer.needsNegotiation()
	case SignalMessageTransceiverRequest:
		var transceiver SignalMessageTransceiver
		_, err := peer.connection.AddTransceiverFromKind(transceiver.Kind, transceiver.Init...)
		return err
	case SignalMessageCandidate:
		candidateJSON, ok := message["candidate"].(map[string]interface{})
		if !ok {
			return errInvalidSignalMessage
		}
		var candidate webrtc.ICECandidateInit
		if candidateRaw, ok := candidateJSON["candidate"].(string); ok {
			candidate.Candidate = candidateRaw
		} else {
			return errInvalidSignalMessage
		}
		if sdpMidRaw, ok := candidateJSON["sdpMid"].(string); ok {
			candidate.SDPMid = &sdpMidRaw
		}
		if sdpMLineIndexRaw, ok := candidateJSON["sdpMLineIndex"].(float64); ok {
			sdpMLineIndex := uint16(sdpMLineIndexRaw)
			candidate.SDPMLineIndex = &sdpMLineIndex
		}
		if usernameFragmentRaw, ok := candidateJSON["usernameFragment"].(string); ok {
			candidate.UsernameFragment = &usernameFragmentRaw
		}
		if peer.connection.RemoteDescription() == nil {
			peer.pendingCandidates = append(peer.pendingCandidates, candidate)
			return nil
		} else {
			return peer.connection.AddICECandidate(candidate)
		}
	case SignalMessageAnswer:
		fallthrough
	case SignalMessageOffer:
		fallthrough
	case SignalMessagePRAnswer:
		fallthrough
	case SignalMessageRollback:
		sdpRaw, ok := message["sdp"].(string)
		if !ok {
			return errInvalidSignalMessage
		}
		sdp := webrtc.SessionDescription{
			Type: webrtc.NewSDPType(messageType),
			SDP:  sdpRaw,
		}
		slog.Debug(fmt.Sprintf("%s: setting remote sdp", peer.id))
		if err := peer.connection.SetRemoteDescription(sdp); err != nil {
			return err
		}
		peer.pendingCandidatesLock.Lock()
		for _, candidate := range peer.pendingCandidates {
			if err := peer.connection.AddICECandidate(candidate); err != nil {
				slog.Error(fmt.Sprintf("%s: error adding ice candidate: %s", peer.id, err))
			}
		}
		peer.pendingCandidates = nil
		peer.pendingCandidatesLock.Unlock()
		if peer.connection.RemoteDescription().Type == webrtc.SDPTypeOffer {
			err := peer.createAnswer()
			if err != nil {
				return err
			}
		}
		return nil
	default:
		slog.Debug(fmt.Sprintf("%s: invalid signal type: %+v", peer.id, message))
		return errInvalidSignalMessageType
	}
}

func (peer *Peer) Close() error {
	return peer.close(false)
}

func (peer *Peer) close(triggerCallbacks bool) error {
	var err1, err2 error
	if peer.connection != nil {
		err1 = peer.connection.Close()
		peer.connection = nil
		triggerCallbacks = true
	}
	if peer.channel != nil {
		err2 = peer.channel.Close()
		peer.channel = nil
		triggerCallbacks = true
	}
	if triggerCallbacks {
		for _, fn := range peer.onClose {
			fn()
		}
	}
	return errors.Join(err1, err2)
}

func (peer *Peer) createPeer() error {
	err := peer.close(false)
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
			"type": SignalMessageRenegotiate,
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
	offerJSON, err := toJSON(offer)
	if err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: created offer: %+v", peer.id, offerJSON))
	return peer.onSignal(offerJSON)
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
	answerJSON, err := toJSON(answer)
	if err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: created answer: %+v", peer.id, answerJSON))
	return peer.onSignal(answerJSON)
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
		peer.close(true)
	case webrtc.PeerConnectionStateFailed:
		slog.Debug(fmt.Sprintf("%s: connection failed", peer.id))
		peer.close(true)
	case webrtc.PeerConnectionStateClosed:
		slog.Debug(fmt.Sprintf("%s: connection closed", peer.id))
		peer.close(true)
	}
}

func (peer *Peer) onICECandidate(pendingCandidate *webrtc.ICECandidate) {
	if pendingCandidate == nil {
		return
	}
	if peer.connection.RemoteDescription() == nil {
		peer.pendingCandidatesLock.Lock()
		peer.pendingCandidates = append(peer.pendingCandidates, pendingCandidate.ToJSON())
		peer.pendingCandidatesLock.Unlock()
	} else {
		iceCandidateInit := pendingCandidate.ToJSON()
		iceCandidateInitJSON, err := toJSON(iceCandidateInit)
		if err != nil {
			slog.Error(fmt.Sprintf("%s: error marshaling ice candidate: %s", peer.id, err))
			return
		}
		if err := peer.onSignal(SignalMessage{
			"type":      SignalMessageCandidate,
			"candidate": iceCandidateInitJSON,
		}); err != nil {
			slog.Error(fmt.Sprintf("%s: error sending ice candidate: %s", peer.id, err))
		}
	}
}

func (peer *Peer) onDataChannel(dc *webrtc.DataChannel) {
	peer.channel = dc
	peer.channel.OnError(peer.onDataChannelError)
	peer.channel.OnOpen(peer.onDataChannelOpen)
	peer.channel.OnMessage(peer.onDataChannelMessage)
}

func toJSON(v interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}
