package syncer

import (
	"encoding/json"
	"math/rand"
	"os"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/route-emitter/diegonats"
	"code.cloudfoundry.org/route-emitter/routingtable"
	"github.com/nats-io/nats"
	uuid "github.com/nu7hatch/gouuid"
)

type NatsSyncer struct {
	natsClient                    diegonats.NATSClient
	clock                         clock.Clock
	syncInterval                  time.Duration
	events                        Events
	routerStart                   chan time.Duration
	internalRouterStart           chan time.Duration
	waitForInternalRoutesGreeting bool

	logger lager.Logger
}

func NewSyncer(
	clock clock.Clock,
	syncInterval time.Duration,
	natsClient diegonats.NATSClient,
	internalRoutes bool,
	logger lager.Logger,
) *NatsSyncer {
	return &NatsSyncer{
		natsClient: natsClient,

		clock:        clock,
		syncInterval: syncInterval,
		events: Events{
			Sync:         make(chan struct{}, 1),
			Emit:         make(chan struct{}, 1),
			InternalSync: make(chan struct{}, 1),
			InternalEmit: make(chan struct{}, 1),
		},

		routerStart:         make(chan time.Duration),
		internalRouterStart: make(chan time.Duration),

		logger: logger.Session("syncer"),
		waitForInternalRoutesGreeting: internalRoutes,
	}
}

func (s *NatsSyncer) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	s.logger.Info("starting")
	replyUuid, err := uuid.NewV4()
	if err != nil {
		return err
	}

	err = s.listenForRouter(replyUuid.String())
	if err != nil {
		return err
	}

	if s.waitForInternalRoutesGreeting {
		err = s.listenForServiceDiscovery(replyUuid.String())
		if err != nil {
			return err
		}
	}
	close(ready)
	s.logger.Info("started")

	var routerRegisterInterval time.Duration
	retryGreetingTicker := s.clock.NewTicker(time.Second)

	//keep trying to greet until we hear from the router
GREET_LOOP:
	for {
		s.logger.Info("greeting-router")
		err := s.greetRouter(replyUuid.String())
		if err != nil {
			s.logger.Error("failed-to-greet-router", err)
			return err
		}

		select {
		case routerRegisterInterval = <-s.routerStart:
			s.logger.Info("received-router-prune-interval", lager.Data{"interval": routerRegisterInterval.String()})
			break GREET_LOOP
		case <-retryGreetingTicker.C():
		case <-signals:
			s.logger.Info("stopping")
			return nil
		}
	}
	retryGreetingTicker.Stop()

	//greet service discovery
	if s.waitForInternalRoutesGreeting {
		s.logger.Info("greeting-service-discovery")
		err = s.greetServiceDiscovery(replyUuid.String())
		if err != nil {
			s.logger.Error("failed-to-greet-service-discovery", err)
			return err
		}
		s.internalSync()
	}

	s.sync()

	// now keep emitting at the desired interval, syncing every syncInterval
	syncTicker := s.clock.NewTicker(s.syncInterval)
	routerTicker := s.clock.NewTicker(routerRegisterInterval)

	randSource := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		select {
		case routerRegisterInterval = <-s.routerStart:
			s.logger.Info("received-new-router-prune-interval", lager.Data{"interval": routerRegisterInterval.String()})
			jitterInterval := randSource.Int63n(int64(0.2 * float64(routerRegisterInterval)))
			s.clock.Sleep(time.Duration(jitterInterval))
			routerTicker.Stop()
			routerTicker = s.clock.NewTicker(routerRegisterInterval)
			s.emit()
		case <-routerTicker.C():
			s.logger.Info("emitting-routes")
			s.emit()
		case <-syncTicker.C():
			s.logger.Info("syncing")
			s.sync()
		case <-signals:
			s.logger.Info("stopping")
			syncTicker.Stop()
			routerTicker.Stop()
			return nil
		}
	}

	return nil
}

func (s *NatsSyncer) Events() Events {
	return s.events
}

func (s *NatsSyncer) emit() {
	select {
	case s.events.Emit <- struct{}{}:
	default:
		s.logger.Debug("emit-already-in-progress")
	}
}

func (s *NatsSyncer) sync() {
	s.events.Sync <- struct{}{}
}

func (s *NatsSyncer) internalSync() {
	s.events.InternalSync <- struct{}{}
}

func (s *NatsSyncer) listenForRouter(replyUUID string) error {
	_, err := s.natsClient.Subscribe("router.start", s.handleRouterStart)
	if err != nil {
		return err
	}

	sub, err := s.natsClient.Subscribe(replyUUID, s.handleRouterStart)
	if err != nil {
		return err
	}
	sub.AutoUnsubscribe(1)

	return nil
}

func (s *NatsSyncer) greetRouter(replyUUID string) error {
	err := s.natsClient.PublishRequest("router.greet", replyUUID, []byte{})
	if err != nil {
		return err
	}

	return nil
}

func (s *NatsSyncer) listenForServiceDiscovery(replyUUID string) error {
	_, err := s.natsClient.Subscribe("service-discovery.start", s.handleServiceDiscoveryStart)
	if err != nil {
		return err
	}

	// sub, err := s.natsClient.Subscribe(replyUUID, s.handleServiceDiscoveryStart)
	// if err != nil {
	// 	return err
	// }
	// sub.AutoUnsubscribe(1)

	return nil
}

func (s *NatsSyncer) greetServiceDiscovery(replyUUID string) error {
	err := s.natsClient.PublishRequest("service-discovery.greet", "TODO Figure out replyUUID", []byte{})
	if err != nil {
		return err
	}

	return nil
}

func (s *NatsSyncer) handleRouterStart(msg *nats.Msg) {
	var response routingtable.RouterGreetingMessage

	err := json.Unmarshal(msg.Data, &response)
	if err != nil {
		s.logger.Error("received-invalid-router-start", err, lager.Data{
			"payload": msg.Data,
		})
		return
	}

	greetInterval := response.MinimumRegisterInterval
	s.routerStart <- time.Duration(greetInterval) * time.Second
}

func (s *NatsSyncer) handleServiceDiscoveryStart(msg *nats.Msg) {
	var response routingtable.RouterGreetingMessage // TODO consider changing the name of this struct - it's used by both router and service-discovery

	err := json.Unmarshal(msg.Data, &response)
	if err != nil {
		s.logger.Error("received-invalid-service-discovery-start", err, lager.Data{
			"payload": msg.Data,
		})
		return
	}

	greetInterval := response.MinimumRegisterInterval
	s.internalRouterStart <- time.Duration(greetInterval) * time.Second
}
