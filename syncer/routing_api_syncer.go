package syncer

import (
	"os"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/lager"
)

type RoutingAPISyncer struct {
	clock        clock.Clock
	syncInterval time.Duration
	syncChannel  chan struct{}
	logger       lager.Logger
}

func NewRoutingApiSyncer(
	clock clock.Clock,
	syncInterval time.Duration,
	syncChannel chan struct{},
	logger lager.Logger,
) *RoutingAPISyncer {
	return &RoutingAPISyncer{
		clock:        clock,
		syncInterval: syncInterval,
		syncChannel:  syncChannel,

		logger: logger.Session("routing-api-syncer"),
	}
}

func (s *RoutingAPISyncer) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)
	s.logger.Info("started")

	s.logger.Debug("starting-initial-sync")
	s.sync()

	//now keep emitting at the desired interval, syncing with etcd every syncInterval
	syncTicker := s.clock.NewTicker(s.syncInterval)

	for {
		select {
		case <-syncTicker.C():
			s.logger.Debug("syncing")
			s.sync()
		case <-signals:
			s.logger.Info("stopping")
			syncTicker.Stop()
			return nil
		}
	}
}

func (s *RoutingAPISyncer) sync() {
	select {
	case s.syncChannel <- struct{}{}:
	default:
		s.logger.Debug("sync-already-in-progress")
	}
}