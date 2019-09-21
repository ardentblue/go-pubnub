package pubnub

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ardentblue/go-pubnub/utils"
)

// SubscriptionManager Events:
// - ConnectedCategory - after connection established
// - DisconnectedCategory - after subscription loop stops for any reason (no
// channels left or error happened)

// Unsubscribe.
// When you unsubscribe from channel or channel group the following events
// happens:
// - LoopStopCategory - immediately when no more channels or channel groups left
// to subscribe
// - PNUnsubscribeOperation - after leave request was fulfilled and server is
// notified about unsubscibed items
//
// Announcement
// Status, Message and Presence announcement happens in a distinct goroutine.
// It doesn't block subscribe loop.
// Keep in mind that each listener will receive the same pointer to a response
// object. You may wish to create a shallow copy of either the response or the
// response message by you own to not affect the other listeners.

// Heartbeat:
// - Heartbeat is enabled by default.
// - Default presence timeout is 300 seconds.
// - The first Heartbeat request will be scheduled to be executed after
// getHeartbeatInterval() seconds (default - 149).
type SubscriptionManager struct {
	sync.RWMutex

	subscriptionLock sync.Mutex

	listenerManager     *ListenerManager
	stateManager        *StateManager
	pubnub              *PubNub
	reconnectionManager *ReconnectionManager
	transport           http.RoundTripper

	hbLoopMutex sync.RWMutex
	hbDataMutex sync.RWMutex
	hbTimer     *time.Ticker
	hbDone      chan bool

	messages        chan subscribeMessage
	ctx             Context
	subscribeCancel func()
	heartbeatCancel func()

	// Store the latest timetoken to subscribe with, null by default to get the
	// latest timetoken.
	timetoken int64

	// When changing the channel mix, store the timetoken for a later date
	storedTimetoken int64

	region int8

	subscriptionStateAnnounced   bool
	heartbeatStopCalled          bool
	exitSubscriptionManagerMutex sync.Mutex
	exitSubscriptionManager      chan bool
}

// SubscribeOperation
type SubscribeOperation struct {
	Channels         []string
	ChannelGroups    []string
	PresenceEnabled  bool
	Timetoken        int64
	FilterExpression string
	State            map[string]interface{}
}

type UnsubscribeOperation struct {
	Channels      []string
	ChannelGroups []string
}

type StateOperation struct {
	channels      []string
	channelGroups []string
	state         map[string]interface{}
}

func newSubscriptionManager(pubnub *PubNub, ctx Context) *SubscriptionManager {
	manager := &SubscriptionManager{}

	manager.pubnub = pubnub

	manager.listenerManager = newListenerManager(ctx, pubnub)
	manager.stateManager = newStateManager()

	manager.Lock()
	manager.timetoken = 0
	manager.storedTimetoken = -1
	manager.subscriptionStateAnnounced = false
	manager.ctx, manager.subscribeCancel = contextWithCancel(backgroundContext)
	manager.messages = make(chan subscribeMessage, 1000)
	manager.reconnectionManager = newReconnectionManager(pubnub)

	manager.Unlock()

	if manager.pubnub.Config.PNReconnectionPolicy != PNNonePolicy {

		manager.reconnectionManager.HandleReconnection(func() {
			go manager.reconnect()

			manager.Lock()
			manager.subscriptionStateAnnounced = true
			manager.Unlock()
			combinedChannels := manager.stateManager.prepareChannelList(true)
			combinedGroups := manager.stateManager.prepareGroupList(true)

			pnStatus := &PNStatus{
				Error:                 false,
				AffectedChannels:      combinedChannels,
				AffectedChannelGroups: combinedGroups,
				Category:              PNReconnectedCategory,
			}

			pubnub.Config.Log.Println("Status: ", pnStatus)

			manager.listenerManager.announceStatus(pnStatus)
		})
	}

	manager.reconnectionManager.HandleOnMaxReconnectionExhaustion(func() {
		combinedChannels := manager.stateManager.prepareChannelList(true)
		combinedGroups := manager.stateManager.prepareGroupList(true)

		pnStatus := &PNStatus{
			Error:                 false,
			AffectedChannels:      combinedChannels,
			AffectedChannelGroups: combinedGroups,
			Category:              PNReconnectionAttemptsExhausted,
		}
		pubnub.Config.Log.Println("Status: ", pnStatus)

		manager.listenerManager.announceStatus(pnStatus)

		manager.Disconnect()
	})

	// actions:
	// add channel
	// remove channel
	// add listener
	// remove listener
	// unsubscribe all
	// cancel
	// addListeners := func()
	return manager
}

func (m *SubscriptionManager) Destroy() {
	m.subscribeCancel()
	if m.exitSubscriptionManager != nil {
		close(m.exitSubscriptionManager)
	}
	if m.listenerManager.exitListener != nil {
		close(m.listenerManager.exitListener)
	}
	if m.reconnectionManager.exitReconnectionManager != nil {
		close(m.reconnectionManager.exitReconnectionManager)
	}
}

func (m *SubscriptionManager) adaptState(stateOperation StateOperation) {
	m.stateManager.adaptStateOperation(stateOperation)
}

func (m *SubscriptionManager) adaptSubscribe(
	subscribeOperation *SubscribeOperation) {
	m.stateManager.adaptSubscribeOperation(subscribeOperation)
	m.pubnub.Config.Log.Println("adapting a new subscription", subscribeOperation.Channels,
		subscribeOperation.PresenceEnabled)

	m.Lock()

	m.subscriptionStateAnnounced = false

	if subscribeOperation.Timetoken != 0 {
		m.timetoken = subscribeOperation.Timetoken
	}

	if m.timetoken != 0 {
		m.storedTimetoken = m.timetoken
	}

	m.timetoken = 0

	m.Unlock()

	m.reconnect()
}

func (m *SubscriptionManager) adaptUnsubscribe(
	unsubscribeOperation *UnsubscribeOperation) {
	m.pubnub.Config.Log.Println("before adaptUnsubscribeOperation")
	m.stateManager.adaptUnsubscribeOperation(unsubscribeOperation)
	m.pubnub.Config.Log.Println("after adaptUnsubscribeOperation")

	m.Lock()
	m.subscriptionStateAnnounced = false
	m.Unlock()

	go func() {
		announceAck := false
		if !m.pubnub.Config.SuppressLeaveEvents {
			_, err := m.pubnub.Leave().Channels(unsubscribeOperation.Channels).
				ChannelGroups(unsubscribeOperation.ChannelGroups).Execute()

			if err != nil {
				pnStatus := &PNStatus{
					Category:              PNBadRequestCategory,
					ErrorData:             err,
					Error:                 true,
					Operation:             PNUnsubscribeOperation,
					AffectedChannels:      unsubscribeOperation.Channels,
					AffectedChannelGroups: unsubscribeOperation.ChannelGroups,
				}
				m.pubnub.Config.Log.Println("Leave: err", err, pnStatus)
				m.listenerManager.announceStatus(pnStatus)
			} else {
				announceAck = true
			}
		} else {
			announceAck = true
		}

		if announceAck {
			pnStatus := &PNStatus{
				Category:              PNAcknowledgmentCategory,
				StatusCode:            200,
				Operation:             PNUnsubscribeOperation,
				UUID:                  m.pubnub.Config.UUID,
				AffectedChannels:      unsubscribeOperation.Channels,
				AffectedChannelGroups: unsubscribeOperation.ChannelGroups,
			}
			m.pubnub.Config.Log.Println("Leave: ack", pnStatus)
			m.listenerManager.announceStatus(pnStatus)
			m.pubnub.Config.Log.Println("After Leave: ack", pnStatus)
		}
	}()
	m.pubnub.Config.Log.Println("before storedTimetoken reset")
	m.Lock()
	if m.stateManager.isEmpty() {
		m.region = 0
		m.storedTimetoken = -1
		m.timetoken = 0
	} else {
		m.storedTimetoken = m.timetoken
		m.timetoken = 0
	}
	m.Unlock()
	m.pubnub.Config.Log.Println("after storedTimetoken reset")

	m.reconnect()
	m.pubnub.Config.Log.Println("after reconnect")
}

func (m *SubscriptionManager) startSubscribeLoop() {
	m.pubnub.Config.Log.Println("startSubscribeLoop")
	go subscribeMessageWorker(m)

	go m.reconnectionManager.startPolling()

	for {
		m.pubnub.Config.Log.Println("startSubscribeLoop looping...")
		combinedChannels := m.stateManager.prepareChannelList(true)
		combinedGroups := m.stateManager.prepareGroupList(true)

		if len(combinedChannels) == 0 && len(combinedGroups) == 0 {
			m.listenerManager.announceStatus(&PNStatus{
				Category: PNDisconnectedCategory,
			})
			m.pubnub.Config.Log.Println("no channels left to subscribe")
			m.reconnectionManager.stopHeartbeatTimer()

			break
		}

		m.Lock()
		tt := m.timetoken
		ctx := m.ctx
		m.Unlock()

		opts := &subscribeOpts{
			pubnub:           m.pubnub,
			Channels:         combinedChannels,
			ChannelGroups:    combinedGroups,
			Timetoken:        tt,
			Heartbeat:        m.pubnub.Config.PresenceTimeout,
			FilterExpression: m.pubnub.Config.FilterExpression,
			ctx:              ctx,
		}

		if s := m.stateManager.createStatePayload(); len(s) > 0 {
			opts.State = s
		}
		res, _, err := executeRequest(opts)
		if err != nil {
			m.pubnub.Config.Log.Println(err.Error())

			if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "request canceled") {
				m.listenerManager.announceStatus(&PNStatus{
					Category: PNTimeoutCategory,
				})
				m.pubnub.Config.Log.Println("continue")
				continue
			} else {

				if strings.Contains(err.Error(), "context canceled") {
					pnStatus := &PNStatus{
						Category: PNCancelledCategory,
					}
					m.pubnub.Config.Log.Println("Status:", pnStatus)
					m.listenerManager.announceStatus(pnStatus)
					m.pubnub.Config.Log.Println("context canceled")
					return
				} else if strings.Contains(err.Error(), "Forbidden") ||
					strings.Contains(err.Error(), "403") {
					pnStatus := &PNStatus{
						Category: PNAccessDeniedCategory,
					}
					m.pubnub.Config.Log.Println("Status:", pnStatus)
					m.listenerManager.announceStatus(pnStatus)
					// RIOT.
					continue
					// m.unsubscribeAll()
					// break
				} else if strings.Contains(err.Error(), "400") ||
					strings.Contains(err.Error(), "Bad Request") {
					pnStatus := &PNStatus{
						Category: PNBadRequestCategory,
					}
					m.pubnub.Config.Log.Println("Status:", pnStatus)
					m.listenerManager.announceStatus(pnStatus)
					// RIOT.
					continue
					// m.unsubscribeAll()
					// break
				} else if strings.Contains(err.Error(), "530") || strings.Contains(err.Error(), "No Stub Matched") {
					pnStatus := &PNStatus{
						Category: PNNoStubMatchedCategory,
					}
					m.pubnub.Config.Log.Println("Status:", pnStatus)
					m.listenerManager.announceStatus(pnStatus)
					// RIOT.
					continue
					//m.unsubscribeAll()
					//break
				} else {
					pnStatus := &PNStatus{
						Category: PNUnknownCategory,
					}
					m.pubnub.Config.Log.Println("Status:", pnStatus)
					m.listenerManager.announceStatus(pnStatus)
					// RIOT.
					continue

					//break
				}
			}

		}

		m.Lock()
		announced := m.subscriptionStateAnnounced

		if announced == false {
			// RIOT - Need affected channels
			m.listenerManager.announceStatus(&PNStatus{
				Category:         PNConnectedCategory,
				AffectedChannels: combinedChannels,
			})
			m.subscriptionStateAnnounced = true
		}
		m.Unlock()

		var envelope subscribeEnvelope
		err = json.Unmarshal(res, &envelope)
		if err != nil {
			pnStatus := &PNStatus{
				Category:              PNBadRequestCategory,
				ErrorData:             err,
				Error:                 true,
				Operation:             PNSubscribeOperation,
				AffectedChannels:      combinedChannels,
				AffectedChannelGroups: combinedGroups,
			}
			m.pubnub.Config.Log.Println("Unmarshal: err", err, pnStatus)

			m.listenerManager.announceStatus(pnStatus)
		}
		messageCount := len(envelope.Messages)
		if messageCount > 0 {
			if messageCount > m.pubnub.Config.MessageQueueOverflowCount {
				pnStatus := &PNStatus{
					Error:                 false,
					AffectedChannels:      combinedChannels,
					AffectedChannelGroups: combinedGroups,
					Category:              PNRequestMessageCountExceededCategory,
				}
				m.pubnub.Config.Log.Println("Status: ", pnStatus)

				m.listenerManager.announceStatus(pnStatus)
			}
			for _, message := range envelope.Messages {
				m.messages <- message
			}
		}

		m.Lock()
		if m.storedTimetoken != -1 {

			m.timetoken = m.storedTimetoken
			m.storedTimetoken = -1
		} else {
			tt, err := strconv.ParseInt(envelope.Metadata.Timetoken, 10, 64)
			if err != nil {

				pnStatus := &PNStatus{
					Category:              PNBadRequestCategory,
					ErrorData:             err,
					Error:                 true,
					Operation:             PNSubscribeOperation,
					AffectedChannels:      combinedChannels,
					AffectedChannelGroups: combinedGroups,
				}
				m.pubnub.Config.Log.Println("ParseInt: err", err, pnStatus)
				m.listenerManager.announceStatus(pnStatus)
			}

			m.timetoken = tt
		}

		m.region = envelope.Metadata.Region
		m.Unlock()
	}
}

func (m *SubscriptionManager) startHeartbeatTimer() {
	m.stopHeartbeat()
	m.pubnub.Config.Log.Println("heartbeat: new timer", m.pubnub.Config.HeartbeatInterval)
	if m.pubnub.Config.PresenceTimeout <= 0 && m.pubnub.Config.HeartbeatInterval <= 0 {
		return
	}

	m.hbLoopMutex.Lock()
	m.hbDataMutex.Lock()
	m.hbDone = make(chan bool)
	m.hbTimer = time.NewTicker(time.Duration(m.pubnub.Config.HeartbeatInterval) * time.Second)
	m.hbDataMutex.Unlock()

	go func() {
		defer m.hbLoopMutex.Unlock()
		defer func() {
			m.hbDataMutex.Lock()
			m.hbDone = nil
			m.hbDataMutex.Unlock()
		}()

		for {
			m.hbDataMutex.RLock()
			timerCh := m.hbTimer.C
			doneCh := m.hbDone

			m.hbDataMutex.RUnlock()

			select {
			case <-timerCh:
				m.performHeartbeatLoop()
			case <-doneCh:
				m.log("heartbeat: loop: after stop")
				return
			}
		}
	}()
}

func (m *SubscriptionManager) stopHeartbeat() {
	m.log("heartbeat: loop: stopping...")

	m.hbDataMutex.Lock()
	if m.hbTimer != nil {
		m.hbTimer.Stop()
		m.pubnub.Config.Log.Println("heartbeat: loop: timer stopped")
	}

	if m.hbDone != nil {
		m.hbDone <- true
		m.pubnub.Config.Log.Println("heartbeat: loop: done channel stopped")
	}
	m.hbDataMutex.Unlock()
}

func (m *SubscriptionManager) performHeartbeatLoop() error {
	presenceChannels := m.stateManager.prepareChannelList(false)
	presenceGroups := m.stateManager.prepareGroupList(false)
	stateStorage := m.stateManager.createStatePayload()

	if m.stateManager.hasNonPresenceChannels() {
		m.pubnub.Config.Log.Println("heartbeat: no channels left")
		go m.stopHeartbeat()
		return nil
	}
	m.stateManager.RLock()
	m.pubnub.Config.Log.Println(len(m.stateManager.channels), len(m.stateManager.groups), len(m.stateManager.presenceChannels), len(m.stateManager.presenceGroups))
	m.stateManager.RUnlock()

	_, status, err := newHeartbeatBuilder(m.pubnub).
		Channels(presenceChannels).
		ChannelGroups(presenceGroups).
		State(stateStorage).
		Execute()

	if err != nil {

		pnStatus := &PNStatus{
			Operation: PNHeartBeatOperation,
			Category:  PNBadRequestCategory,
			Error:     true,
			ErrorData: err,
		}
		m.pubnub.Config.Log.Println("performHeartbeatLoop: err", err, pnStatus)

		m.listenerManager.announceStatus(pnStatus)

		return err
	}

	pnStatus := &PNStatus{
		Category:   PNUnknownCategory,
		Error:      false,
		Operation:  PNHeartBeatOperation,
		StatusCode: status.StatusCode,
	}
	m.pubnub.Config.Log.Println("performHeartbeatLoop: err", err, pnStatus)

	m.listenerManager.announceStatus(pnStatus)

	return nil
}

type subscribeEnvelope struct {
	Messages []subscribeMessage `json:"m"`
	Metadata struct {
		Timetoken string `json:"t"`
		Region    int8   `json:"r"`
	} `json:"t"`
}

type subscribeMessage struct {
	Shard             string      `json:"a"`
	SubscriptionMatch string      `json:"b"`
	Channel           string      `json:"c"`
	IssuingClientID   string      `json:"i"`
	SubscribeKey      string      `json:"k"`
	Flags             int         `json:"f"`
	Payload           interface{} `json:"d"`
	UserMetadata      interface{} `json:"u"`

	PublishMetaData publishMetadata `json:"p"`
}

type presenceEnvelope struct {
	Action    string
	UUID      string
	Occupancy int
	Timestamp int64
	Data      interface{}
}

type publishMetadata struct {
	PublishTimetoken string `json:"t"`
	Region           int    `json:"r"`
}

type originationMetadata struct {
	Timetoken int64 `json:"t"`
	Region    int   `json:"r"`
}

func subscribeMessageWorker(m *SubscriptionManager) {
	m.Lock()
	if m.ctx == nil && m.subscribeCancel == nil {
		m.pubnub.Config.Log.Println("subscribeMessageWorker setting context")
		m.ctx, m.subscribeCancel = contextWithCancel(backgroundContext)
		m.pubnub.Config.Log.Println("subscribeMessageWorker after setting context")
	}

	m.pubnub.Config.Log.Println("subscribeMessageWorker")

	m.Unlock()
	if m.exitSubscriptionManager != nil {
		m.exitSubscriptionManager <- true
		m.pubnub.Config.Log.Println("close exitSubscriptionManager")
	}
	m.pubnub.Config.Log.Println("acquiring lock exitSubscriptionManagerMutex")
	m.exitSubscriptionManagerMutex.Lock()
	m.pubnub.Config.Log.Println("make channel exitSubscriptionManager")
	m.exitSubscriptionManager = make(chan bool)
	for m.exitSubscriptionManager != nil {
		m.pubnub.Config.Log.Println("subscribeMessageWorker looping...")
		combinedChannels := m.stateManager.prepareChannelList(true)
		combinedGroups := m.stateManager.prepareGroupList(true)

		if len(combinedChannels) == 0 && len(combinedGroups) == 0 {
			m.pubnub.Config.Log.Println("subscribeMessageWorker all channels unsubscribed")
			break
		}
		select {
		case <-m.exitSubscriptionManager:
			m.pubnub.Config.Log.Println("subscribeMessageWorker context done")
			m.exitSubscriptionManager = nil
			break
		case message := <-m.messages:
			m.pubnub.Config.Log.Println("subscribeMessageWorker messages")
			processSubscribePayload(m, message)
		}
	}
	m.pubnub.Config.Log.Println("subscribeMessageWorker after for")
	m.exitSubscriptionManagerMutex.Unlock()
}

func processSubscribePayload(m *SubscriptionManager, payload subscribeMessage) {
	channel := payload.Channel
	subscriptionMatch := payload.SubscriptionMatch
	publishMetadata := payload.PublishMetaData

	if channel != "" && channel == subscriptionMatch {
		subscriptionMatch = ""
	}

	if strings.Contains(payload.Channel, "-pnpres") {
		var presencePayload map[string]interface{}
		var action, uuid, actualChannel, subscribedChannel string
		var occupancy int
		var timestamp int64
		var data interface{}
		var ok, hereNowRefresh bool

		if presencePayload, ok = payload.Payload.(map[string]interface{}); !ok {
			m.listenerManager.announceStatus(&PNStatus{
				Category:         PNUnknownCategory,
				ErrorData:        errors.New("Response presence parsing error"),
				Error:            true,
				Operation:        PNSubscribeOperation,
				AffectedChannels: []string{channel},
			})
		}

		action, _ = presencePayload["action"].(string)
		uuid, _ = presencePayload["uuid"].(string)
		occupancy, _ = presencePayload["occupancy"].(int)
		if presencePayload["timestamp"] != nil {
			m.pubnub.Config.Log.Println("presencePayload['timestamp'] type", reflect.TypeOf(presencePayload["timestamp"]).Kind())
			switch presencePayload["timestamp"].(type) {
			case int:
				timestamp = int64(presencePayload["timestamp"].(int))
				break
			case float64:
				timestamp = int64(presencePayload["timestamp"].(float64))
				break
			}

		}

		data = presencePayload["data"]
		if presencePayload["here_now_refresh"] != nil {
			hereNowRefresh = presencePayload["here_now_refresh"].(bool)
		}
		timetoken, _ := strconv.ParseInt(publishMetadata.PublishTimetoken, 10, 64)

		strippedPresenceChannel := ""
		strippedPresenceSubscription := ""

		if channel != "" {
			strippedPresenceChannel = strings.Replace(channel, "-pnpres", "", -1)
		}

		if subscriptionMatch != "" {
			actualChannel = channel
			subscribedChannel = subscriptionMatch
			strippedPresenceSubscription = strings.Replace(subscriptionMatch, "-pnpres", "", -1)
		} else {
			subscribedChannel = channel
		}

		pnPresenceResult := &PNPresence{
			Event:             action,
			ActualChannel:     actualChannel,
			SubscribedChannel: subscribedChannel,
			Channel:           strippedPresenceChannel,
			Subscription:      strippedPresenceSubscription,
			State:             data,
			Timetoken:         timetoken,
			Occupancy:         occupancy,
			UUID:              uuid,
			Timestamp:         timestamp,
			HereNowRefresh:    hereNowRefresh,
		}
		m.listenerManager.announcePresence(pnPresenceResult)
	} else {
		actualCh := ""
		subscribedCh := channel
		timetoken, _ := strconv.ParseInt(publishMetadata.PublishTimetoken, 10, 64)

		if subscriptionMatch != "" {
			actualCh = channel
			subscribedCh = subscriptionMatch
		}
		messagePayload, err := parseCipherInterface(payload.Payload, m.pubnub.Config)

		if err != nil {
			pnStatus := &PNStatus{
				Category:         PNBadRequestCategory,
				ErrorData:        err,
				Error:            true,
				Operation:        PNSubscribeOperation,
				AffectedChannels: []string{channel},
			}
			m.pubnub.Config.Log.Println("DecryptString: err", err, pnStatus)
			m.listenerManager.announceStatus(pnStatus)
		}

		pnMessageResult := &PNMessage{
			Message:           messagePayload,
			ActualChannel:     actualCh,
			SubscribedChannel: subscribedCh,
			Channel:           channel,
			Subscription:      subscriptionMatch,
			Timetoken:         timetoken,
			Publisher:         payload.IssuingClientID,
			UserMetadata:      payload.UserMetadata,
		}
		m.pubnub.Config.Log.Println("announceMessage,", pnMessageResult)
		m.listenerManager.announceMessage(pnMessageResult)
		m.pubnub.Config.Log.Println("after announceMessage")
	}
}

// parseCipherInterface handles the decryption in case a cipher key is used
// in case of error it returns data as is.
//
// parameters
// data: the data to decrypt as interface.
// cipherKey: cipher key to use to decrypt.
//
// returns the decrypted data as interface and error.
func parseCipherInterface(data interface{}, pnConf *Config) (interface{}, error) {
	if pnConf.CipherKey != "" {
		pnConf.Log.Println("reflect.TypeOf(data).Kind()", reflect.TypeOf(data).Kind(), data)
		switch v := data.(type) {
		case map[string]interface{}:

			if !pnConf.DisablePNOtherProcessing {
				//decrypt pn_other only
				msg, ok := v["pn_other"].(string)
				if ok {
					pnConf.Log.Println("v[pn_other]", v["pn_other"], v, msg)
					decrypted, errDecryption := utils.DecryptString(pnConf.CipherKey, msg)
					if errDecryption != nil {
						pnConf.Log.Println(errDecryption, msg)
						return v, errDecryption
					} else {
						var intf interface{}
						err := json.Unmarshal([]byte(decrypted.(string)), &intf)
						if err != nil {
							pnConf.Log.Println("Unmarshal: err", err)
							return intf, err
						}
						v["pn_other"] = intf

						pnConf.Log.Println("reflect.TypeOf(v).Kind()", reflect.TypeOf(v).Kind(), v)
						return v, nil
					}
				}
				return v, nil
			}
			pnConf.Log.Println("return as is reflect.TypeOf(v).Kind()", reflect.TypeOf(v).Kind(), v)
			return v, nil
		case string:
			var intf interface{}
			decrypted, errDecryption := utils.DecryptString(pnConf.CipherKey, data.(string))
			if errDecryption != nil {
				pnConf.Log.Println(errDecryption, intf)
				intf = data
				return intf, errDecryption
			}
			pnConf.Log.Println("reflect.TypeOf(intf).Kind()", reflect.TypeOf(decrypted).Kind(), decrypted)

			err := json.Unmarshal([]byte(decrypted.(string)), &intf)
			if err != nil {
				pnConf.Log.Println("Unmarshal: err", err)
				return intf, err
			}

			return intf, nil
		default:
			pnConf.Log.Println("returning as is", reflect.TypeOf(v).Kind())
			return v, nil
		}
	} else {
		pnConf.Log.Println("No Cipher, returning as is ", data)
		return data, nil
	}
}

func (m *SubscriptionManager) AddListener(listener *Listener) {
	m.listenerManager.addListener(listener)
}

func (m *SubscriptionManager) RemoveListener(listener *Listener) {
	m.listenerManager.Lock()
	m.listenerManager.removeListener(listener)
	m.listenerManager.Unlock()
}

func (m *SubscriptionManager) RemoveAllListeners() {
	m.listenerManager.removeAllListeners()
}

func (m *SubscriptionManager) GetListeners() map[*Listener]bool {
	listn := m.listenerManager.listeners
	return listn
}

func (m *SubscriptionManager) reconnect() {
	m.pubnub.Config.Log.Println("reconnect")
	m.reconnectionManager.stopHeartbeatTimer()
	m.pubnub.Config.Log.Println("after stopHeartbeatTimer")
	m.stopSubscribeLoop()

	combinedChannels := m.stateManager.prepareChannelList(true)
	combinedGroups := m.stateManager.prepareGroupList(true)

	if len(combinedChannels) == 0 && len(combinedGroups) == 0 {
		m.pubnub.Config.Log.Println("All channels or channel groups unsubscribed.")
	} else {
		go m.startSubscribeLoop()
		go m.startHeartbeatTimer()
	}
}

func (m *SubscriptionManager) Disconnect() {
	m.pubnub.Config.Log.Println("disconnect")

	if m.exitSubscriptionManager != nil {
		m.exitSubscriptionManager <- true
	}
	m.reconnectionManager.stopHeartbeatTimer()

	m.stopHeartbeat()
	m.unsubscribeAll()
	m.stopSubscribeLoop()

}

func (m *SubscriptionManager) stopSubscribeLoop() {
	m.log("loop stop")

	if m.ctx != nil && m.subscribeCancel != nil {
		m.subscribeCancel()
		m.ctx = nil
		m.subscribeCancel = nil
	}

}

func (m *SubscriptionManager) getSubscribedChannels() []string {
	return m.stateManager.prepareChannelList(false)
}

func (m *SubscriptionManager) getSubscribedGroups() []string {
	return m.stateManager.prepareGroupList(false)
}

func (m *SubscriptionManager) unsubscribeAll() {
	m.adaptUnsubscribe(&UnsubscribeOperation{
		Channels:      m.stateManager.prepareChannelList(true),
		ChannelGroups: m.stateManager.prepareGroupList(true),
	})
}

func (m *SubscriptionManager) log(message string) {
	m.pubnub.Config.Log.Printf("pubnub: subscribe: %s: %s: %s/%s\n",
		message,
		m.pubnub.Config.UUID,
		m.stateManager.prepareChannelList(true),
		m.stateManager.prepareGroupList(true))
}
