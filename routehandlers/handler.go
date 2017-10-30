package routehandlers

import (
	"errors"
	"time"

	"code.cloudfoundry.org/bbs/models"
	loggingclient "code.cloudfoundry.org/diego-logging-client"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/route-emitter/emitter"
	"code.cloudfoundry.org/route-emitter/routingtable"
	"code.cloudfoundry.org/route-emitter/watcher"
)

const (
	routesTotal        = "RoutesTotal"
	routesSynced       = "RoutesSynced"
	routesRegistered   = "RoutesRegistered"
	routesUnregistered = "RoutesUnregistered"
	httpRouteCount     = "HTTPRouteCount"
	tcpRouteCount      = "TCPRouteCount"
)

type Handler struct {
	routingTable      routingtable.RoutingTable
	natsEmitter       emitter.NATSEmitter
	routingAPIEmitter emitter.RoutingAPIEmitter
	localMode         bool
	metronClient      loggingclient.IngressClient
}

var _ watcher.RouteHandler = new(Handler)

func NewHandler(routingTable routingtable.RoutingTable, natsEmitter emitter.NATSEmitter, routingAPIEmitter emitter.RoutingAPIEmitter, localMode bool, metronClient loggingclient.IngressClient) *Handler {
	return &Handler{
		routingTable:      routingTable,
		natsEmitter:       natsEmitter,
		routingAPIEmitter: routingAPIEmitter,
		localMode:         localMode,
		metronClient:      metronClient,
	}
}

func (handler *Handler) HandleEvent(logger lager.Logger, event models.Event) {
	switch event := event.(type) {
	case *models.DesiredLRPCreatedEvent:
		desiredInfo := event.DesiredLrp.DesiredLRPSchedulingInfo()
		handler.handleDesiredCreate(logger, &desiredInfo)
	case *models.DesiredLRPChangedEvent:
		before := event.Before.DesiredLRPSchedulingInfo()
		after := event.After.DesiredLRPSchedulingInfo()
		handler.handleDesiredUpdate(logger, &before, &after)
	case *models.DesiredLRPRemovedEvent:
		desiredInfo := event.DesiredLrp.DesiredLRPSchedulingInfo()
		handler.handleDesiredDelete(logger, &desiredInfo)
	case *models.ActualLRPCreatedEvent:
		routingInfo := routingtable.NewActualLRPRoutingInfo(event.ActualLrpGroup)
		handler.handleActualCreate(logger, routingInfo)
	case *models.ActualLRPChangedEvent:
		before := routingtable.NewActualLRPRoutingInfo(event.Before)
		after := routingtable.NewActualLRPRoutingInfo(event.After)
		handler.handleActualUpdate(logger, before, after)
	case *models.ActualLRPRemovedEvent:
		routingInfo := routingtable.NewActualLRPRoutingInfo(event.ActualLrpGroup)
		handler.handleActualDelete(logger, routingInfo)
	default:
		logger.Error("did-not-handle-unrecognizable-event", errors.New("unrecognizable-event"), lager.Data{"event-type": event.EventType()})
	}
}

func (handler *Handler) Emit(logger lager.Logger) {
	routingEvents, messagesToEmit := handler.routingTable.GetRoutingEvents()

	logger.Info("emitting-nats-messages", lager.Data{"messages": messagesToEmit})
	if handler.natsEmitter != nil {
		err := handler.natsEmitter.Emit(messagesToEmit)
		if err != nil {
			logger.Error("failed-to-emit-nats-routes", err)
		}
	}

	logger.Info("emitting-routing-api-messages", lager.Data{"messages": routingEvents})
	if handler.routingAPIEmitter != nil {
		err := handler.routingAPIEmitter.Emit(routingEvents)
		if err != nil {
			logger.Error("failed-to-emit-tcp-routes", err)
		}
	}

	err := handler.metronClient.IncrementCounterWithDelta(routesSynced, messagesToEmit.RouteRegistrationCount())
	if err != nil {
		logger.Error("failed-send-routes-synced-count-metric", err)
	}
	err = handler.metronClient.SendMetric(routesTotal, handler.routingTable.HTTPAssociationsCount())
	if err != nil {
		logger.Error("failed-to-send-total-route-count-metric", err)
	}
}

func (handler *Handler) Sync(
	logger lager.Logger,
	desired []*models.DesiredLRPSchedulingInfo,
	actuals []*routingtable.ActualLRPRoutingInfo,
	domains models.DomainSet,
	cachedEvents map[string]models.Event,
) {
	logger = logger.Session("sync")
	logger.Debug("starting")
	defer logger.Debug("completed")

	newTable := routingtable.NewRoutingTable(logger, false, handler.metronClient)

	for _, lrp := range desired {
		newTable.SetRoutes(nil, lrp)
	}

	for _, lrp := range actuals {
		newTable.AddEndpoint(lrp)
	}

	natsEmitter := handler.natsEmitter
	routingAPIEmitter := handler.routingAPIEmitter
	table := handler.routingTable

	handler.natsEmitter = nil
	handler.routingAPIEmitter = nil
	handler.routingTable = newTable

	for _, event := range cachedEvents {
		handler.HandleEvent(logger, event)
	}

	handler.routingTable = table
	handler.natsEmitter = natsEmitter
	handler.routingAPIEmitter = routingAPIEmitter

	// start
	start := time.Now()
	routeMappings, messages := handler.routingTable.Swap(newTable, domains)
	swapDuration := time.Now().Sub(start)
	handler.metronClient.SendDuration("table.swap.time", swapDuration)
	// emit

	logger.Debug("start-emitting-messages", lager.Data{
		"num-registration-messages":            len(messages.RegistrationMessages),
		"num-unregistration-messages":          len(messages.UnregistrationMessages),
		"num-internal-registration-messages":   len(messages.InternalRegistrationMessages),
		"num-internal-unregistration-messages": len(messages.InternalUnregistrationMessages),
	})
	handler.emitMessages(logger, messages, routeMappings)
	logger.Debug("done-emitting-messages", lager.Data{
		"num-registration-messages":            len(messages.RegistrationMessages),
		"num-unregistration-messages":          len(messages.UnregistrationMessages),
		"num-internal-registration-messages":   len(messages.InternalRegistrationMessages),
		"num-internal-unregistration-messages": len(messages.InternalUnregistrationMessages),
	})

	if handler.localMode {
		err := handler.metronClient.SendMetric(httpRouteCount, handler.routingTable.HTTPAssociationsCount())
		if err != nil {
			logger.Error("failed-to-send-http-routes-count-metric", err)
		}
		err = handler.metronClient.SendMetric(tcpRouteCount, handler.routingTable.TCPAssociationsCount())
		if err != nil {
			logger.Error("failed-to-send-tcp-route-count-metric", err)
		}
	}
}

func (handler *Handler) RefreshDesired(logger lager.Logger, desiredInfo []*models.DesiredLRPSchedulingInfo) {
	for _, desiredLRP := range desiredInfo {
		routeMappings, messagesToEmit := handler.routingTable.SetRoutes(nil, desiredLRP)
		handler.emitMessages(logger, messagesToEmit, routeMappings)
	}
}

func (handler *Handler) ShouldRefreshDesired(actual *routingtable.ActualLRPRoutingInfo) bool {
	return !handler.routingTable.HasExternalRoutes(actual)
}

func (handler *Handler) handleDesiredCreate(logger lager.Logger, desiredLRP *models.DesiredLRPSchedulingInfo) {
	logger = logger.Session("handle-desired-create", routingtable.DesiredLRPData(desiredLRP))
	logger.Info("starting")
	defer logger.Info("complete")
	routeMappings, messagesToEmit := handler.routingTable.SetRoutes(nil, desiredLRP)
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *Handler) handleDesiredUpdate(logger lager.Logger, before, after *models.DesiredLRPSchedulingInfo) {
	logger = logger.Session("handling-desired-update", lager.Data{
		"before": routingtable.DesiredLRPData(before),
		"after":  routingtable.DesiredLRPData(after),
	})
	logger.Info("starting")
	defer logger.Info("complete")

	routeMappings, messagesToEmit := handler.routingTable.SetRoutes(before, after)
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *Handler) handleDesiredDelete(logger lager.Logger, schedulingInfo *models.DesiredLRPSchedulingInfo) {
	logger = logger.Session("handling-desired-delete", routingtable.DesiredLRPData(schedulingInfo))
	logger.Info("starting")
	defer logger.Info("complete")
	routeMappings, messagesToEmit := handler.routingTable.RemoveRoutes(schedulingInfo)
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *Handler) handleActualCreate(logger lager.Logger, actualLRPInfo *routingtable.ActualLRPRoutingInfo) {
	logger = logger.Session("handling-actual-create", routingtable.ActualLRPData(actualLRPInfo))
	logger.Info("starting")
	defer logger.Info("complete")
	if actualLRPInfo.ActualLRP.State == models.ActualLRPStateRunning {
		logger.Info("handler-adding-endpoint", lager.Data{"net_info": actualLRPInfo.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit := handler.routingTable.AddEndpoint(actualLRPInfo)
		handler.emitMessages(logger, messagesToEmit, routeMappings)
	}
}

func (handler *Handler) handleActualUpdate(logger lager.Logger, before, after *routingtable.ActualLRPRoutingInfo) {
	logger = logger.Session("handling-actual-update", lager.Data{
		"before": routingtable.ActualLRPData(before),
		"after":  routingtable.ActualLRPData(after),
	})
	logger.Info("starting")
	defer logger.Info("complete")

	var (
		messagesToEmit routingtable.MessagesToEmit
		routeMappings  routingtable.TCPRouteMappings
	)
	switch {
	case after.ActualLRP.State == models.ActualLRPStateRunning:
		logger.Info("handler-adding-endpoint", lager.Data{"net_info": after.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit = handler.routingTable.AddEndpoint(after)
	case after.ActualLRP.State != models.ActualLRPStateRunning && before.ActualLRP.State == models.ActualLRPStateRunning:
		logger.Info("handler-removing-endpoint", lager.Data{"net_info": before.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit = handler.routingTable.RemoveEndpoint(before)
	}
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *Handler) handleActualDelete(logger lager.Logger, actualLRPInfo *routingtable.ActualLRPRoutingInfo) {
	logger = logger.Session("handling-actual-delete", routingtable.ActualLRPData(actualLRPInfo))
	logger.Info("starting")
	defer logger.Info("complete")
	if actualLRPInfo.ActualLRP.State == models.ActualLRPStateRunning {
		logger.Info("handler-removing-endpoint", lager.Data{"net_info": actualLRPInfo.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit := handler.routingTable.RemoveEndpoint(actualLRPInfo)
		handler.emitMessages(logger, messagesToEmit, routeMappings)
	}
}

type set map[interface{}]struct{}

func (set set) contains(value interface{}) bool {
	_, found := set[value]
	return found
}

func (set set) add(value interface{}) {
	set[value] = struct{}{}
}

func (handler *Handler) emitMessages(logger lager.Logger, messagesToEmit routingtable.MessagesToEmit, routeMappings routingtable.TCPRouteMappings) {
	if handler.natsEmitter != nil {
		logger.Debug("emit-messages", lager.Data{"messages": messagesToEmit})
		err := handler.natsEmitter.Emit(messagesToEmit)
		if err != nil {
			logger.Error("failed-to-emit-http-routes", err)
		}
		err = handler.metronClient.IncrementCounterWithDelta(routesRegistered, messagesToEmit.RouteRegistrationCount())
		if err != nil {
			logger.Error("failed-to-emit-registration-message-count", err)
		}
		err = handler.metronClient.IncrementCounterWithDelta(routesUnregistered, messagesToEmit.RouteUnregistrationCount())
		if err != nil {
			logger.Error("failed-to-emit-unregistration-message-count", err)
		}
	} else {
		logger.Info("no-emitter-configured-skipping-emit-messages", lager.Data{"messages": messagesToEmit})
	}

	if handler.routingAPIEmitter != nil {
		err := handler.routingAPIEmitter.Emit(routeMappings)
		if err != nil {
			logger.Error("failed-to-emit-http-routes", err)
		}
	}
}
