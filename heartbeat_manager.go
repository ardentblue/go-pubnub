package pubnub

import (
	"fmt"
	"sync"
	"time"
)

type HeartbeatManager struct {
	sync.RWMutex

	heartbeatChannels map[string]*SubscriptionItem
	heartbeatGroups   map[string]*SubscriptionItem
	pubnub            *PubNub

	hbLoopMutex sync.RWMutex
	hbTimer     *time.Ticker
	hbDone      chan bool
	ctx         Context
}

func newHeartbeatManager(pn *PubNub, context Context) *HeartbeatManager {
	return &HeartbeatManager{
		heartbeatChannels: make(map[string]*SubscriptionItem),
		heartbeatGroups:   make(map[string]*SubscriptionItem),
		ctx:               context,
		pubnub:            pn,
	}
}

func (m *HeartbeatManager) Destroy() {
}

func (m *HeartbeatManager) startHeartbeatTimer() {
	m.stopHeartbeat()
	m.pubnub.Config.Log.Println("heartbeat: new timer", m.pubnub.Config.HeartbeatInterval)
	if m.pubnub.Config.PresenceTimeout <= 0 && m.pubnub.Config.HeartbeatInterval <= 0 {
		return
	}

	m.hbLoopMutex.Lock()
	m.pubnub.subscriptionManager.pubnub.subscriptionManager.hbDataMutex.Lock()
	m.hbDone = make(chan bool)
	m.hbTimer = time.NewTicker(time.Duration(m.pubnub.Config.HeartbeatInterval) * time.Second)
	m.pubnub.subscriptionManager.pubnub.subscriptionManager.hbDataMutex.Unlock()

	go func() {
		defer m.hbLoopMutex.Unlock()
		defer func() {
			m.pubnub.subscriptionManager.hbDataMutex.Lock()
			m.hbDone = nil
			m.pubnub.subscriptionManager.hbDataMutex.Unlock()
		}()

		for {
			m.pubnub.subscriptionManager.hbDataMutex.RLock()
			timerCh := m.hbTimer.C
			doneCh := m.hbDone
			m.pubnub.subscriptionManager.hbDataMutex.RUnlock()

			select {
			case <-timerCh:
				timeNow := time.Now().Unix()
				m.pubnub.subscriptionManager.hbDataMutex.RLock()
				reqSentAt := m.pubnub.subscriptionManager.requestSentAt
				m.pubnub.subscriptionManager.hbDataMutex.RUnlock()
				if reqSentAt > 0 {
					timediff := int64(m.pubnub.Config.HeartbeatInterval) - (timeNow - reqSentAt)
					m.pubnub.Config.Log.Println(fmt.Sprintf("heartbeat timediff: %d", timediff))
					m.pubnub.subscriptionManager.requestSentAt = 0
					if timediff > 10 {
						m.pubnub.subscriptionManager.hbDataMutex.Lock()
						m.hbTimer.Stop()
						m.pubnub.subscriptionManager.hbDataMutex.Unlock()

						m.pubnub.Config.Log.Println(fmt.Sprintf("heartbeat sleeping timediff: %d", timediff))
						time.Sleep(time.Duration(timediff) * time.Second)
						m.pubnub.Config.Log.Println("heartbeat sleep end")
						m.pubnub.subscriptionManager.hbDataMutex.Lock()
						m.hbTimer = time.NewTicker(time.Duration(m.pubnub.Config.HeartbeatInterval) * time.Second)
						m.pubnub.subscriptionManager.hbDataMutex.Unlock()
					}
				}
				m.performHeartbeatLoop()
			case <-doneCh:
				m.pubnub.Config.Log.Println("heartbeat: loop after stop")
				return
			}
		}
	}()
}

func (m *HeartbeatManager) stopHeartbeat() {
	m.pubnub.Config.Log.Println("heartbeat: loop: stopping...")

	m.pubnub.subscriptionManager.hbDataMutex.Lock()
	if m.hbTimer != nil {
		m.hbTimer.Stop()
		m.pubnub.Config.Log.Println("heartbeat: loop: timer stopped")
	}

	if m.hbDone != nil {
		m.hbDone <- true
		m.pubnub.Config.Log.Println("heartbeat: loop: done channel stopped")
	}
	m.pubnub.subscriptionManager.requestSentAt = 0
	m.pubnub.subscriptionManager.hbDataMutex.Unlock()
}

func (m *HeartbeatManager) performHeartbeatLoop() error {
	presenceChannels := m.pubnub.subscriptionManager.stateManager.prepareChannelList(false)
	presenceGroups := m.pubnub.subscriptionManager.stateManager.prepareGroupList(false)
	stateStorage := m.pubnub.subscriptionManager.stateManager.createStatePayload()

	if m.pubnub.subscriptionManager.stateManager.hasNonPresenceChannels() {
		m.pubnub.Config.Log.Println("heartbeat: no channels left")
		go m.stopHeartbeat()
		return nil
	}
	m.pubnub.subscriptionManager.stateManager.RLock()
	m.pubnub.Config.Log.Println(len(m.pubnub.subscriptionManager.stateManager.channels), len(m.pubnub.subscriptionManager.stateManager.groups), len(m.pubnub.subscriptionManager.stateManager.presenceChannels), len(m.pubnub.subscriptionManager.stateManager.presenceGroups))
	m.pubnub.subscriptionManager.stateManager.RUnlock()

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

		m.pubnub.subscriptionManager.listenerManager.announceStatus(pnStatus)

		return err
	}

	pnStatus := &PNStatus{
		Category:   PNUnknownCategory,
		Error:      false,
		Operation:  PNHeartBeatOperation,
		StatusCode: status.StatusCode,
	}
	m.pubnub.Config.Log.Println("performHeartbeatLoop: err", err, pnStatus)

	m.pubnub.subscriptionManager.listenerManager.announceStatus(pnStatus)

	return nil
}
