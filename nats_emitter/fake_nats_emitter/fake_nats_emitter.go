// This file was generated by counterfeiter
package fake_nats_emitter

import (
	"sync"

	"github.com/cloudfoundry-incubator/route-emitter/nats_emitter"
	"github.com/cloudfoundry-incubator/route-emitter/routing_table"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
)

type FakeNATSEmitter struct {
	EmitStub        func(messagesToEmit routing_table.MessagesToEmit, registrationCounter, unregistrationCounter *metric.Counter) error
	emitMutex       sync.RWMutex
	emitArgsForCall []struct {
		messagesToEmit        routing_table.MessagesToEmit
		registrationCounter   *metric.Counter
		unregistrationCounter *metric.Counter
	}
	emitReturns struct {
		result1 error
	}
}

func (fake *FakeNATSEmitter) Emit(messagesToEmit routing_table.MessagesToEmit, registrationCounter *metric.Counter, unregistrationCounter *metric.Counter) error {
	fake.emitMutex.Lock()
	fake.emitArgsForCall = append(fake.emitArgsForCall, struct {
		messagesToEmit        routing_table.MessagesToEmit
		registrationCounter   *metric.Counter
		unregistrationCounter *metric.Counter
	}{messagesToEmit, registrationCounter, unregistrationCounter})
	fake.emitMutex.Unlock()
	if fake.EmitStub != nil {
		return fake.EmitStub(messagesToEmit, registrationCounter, unregistrationCounter)
	} else {
		return fake.emitReturns.result1
	}
}

func (fake *FakeNATSEmitter) EmitCallCount() int {
	fake.emitMutex.RLock()
	defer fake.emitMutex.RUnlock()
	return len(fake.emitArgsForCall)
}

func (fake *FakeNATSEmitter) EmitArgsForCall(i int) (routing_table.MessagesToEmit, *metric.Counter, *metric.Counter) {
	fake.emitMutex.RLock()
	defer fake.emitMutex.RUnlock()
	return fake.emitArgsForCall[i].messagesToEmit, fake.emitArgsForCall[i].registrationCounter, fake.emitArgsForCall[i].unregistrationCounter
}

func (fake *FakeNATSEmitter) EmitReturns(result1 error) {
	fake.EmitStub = nil
	fake.emitReturns = struct {
		result1 error
	}{result1}
}

var _ nats_emitter.NATSEmitter = new(FakeNATSEmitter)
