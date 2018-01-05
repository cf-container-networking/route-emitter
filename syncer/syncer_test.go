package syncer_test

import (
	"os"
	"time"

	"code.cloudfoundry.org/bbs/fake_bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/clock/fakeclock"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"code.cloudfoundry.org/route-emitter/diegonats"
	"code.cloudfoundry.org/route-emitter/syncer"
	"code.cloudfoundry.org/routing-info/cfroutes"
	"github.com/nats-io/nats"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const logGuid = "some-log-guid"

var _ = Describe("NatsSyncer", func() {
	const (
		processGuid   = "process-guid-1"
		containerPort = 8080
		instanceGuid  = "instance-guid-1"
		lrpHost       = "1.2.3.4"
		containerIp   = "2.2.2.2"
	)

	var (
		bbsClient    *fake_bbs.FakeClient
		natsClient   *diegonats.FakeNATSClient
		syncerRunner *syncer.NatsSyncer
		process      ifrit.Process
		clock        *fakeclock.FakeClock
		syncInterval time.Duration

		shutdown chan struct{}

		schedulingInfoResponse *models.DesiredLRPSchedulingInfo
		actualResponses        []*models.ActualLRPGroup

		routerStartMessages           chan<- *nats.Msg
		serviceDiscoveryStartMessages chan<- *nats.Msg

		waitForInternalRoutesGreeting bool
	)

	BeforeEach(func() {
		bbsClient = new(fake_bbs.FakeClient)
		natsClient = diegonats.NewFakeClient()

		clock = fakeclock.NewFakeClock(time.Now())
		syncInterval = 10 * time.Second

		startMessages := make(chan *nats.Msg)
		routerStartMessages = startMessages

		natsClient.WhenSubscribing("router.start", func(callback nats.MsgHandler) error {
			go func() {
				for msg := range startMessages {
					callback(msg)
				}
			}()

			return nil
		})

		startMessages2 := make(chan *nats.Msg)
		serviceDiscoveryStartMessages = startMessages2

		natsClient.WhenSubscribing("service-discovery.start", func(callback nats.MsgHandler) error {
			go func() {
				for msg := range startMessages2 {
					callback(msg)
				}
			}()

			return nil
		})

		schedulingInfoResponse = &models.DesiredLRPSchedulingInfo{
			DesiredLRPKey: models.NewDesiredLRPKey(processGuid, "domain", logGuid),
			Routes:        cfroutes.CFRoutes{{Hostnames: []string{"route-1", "route-2"}, Port: containerPort}}.RoutingInfo(),
		}

		actualResponses = []*models.ActualLRPGroup{
			{
				Instance: &models.ActualLRP{
					ActualLRPKey:         models.NewActualLRPKey(processGuid, 1, "domain"),
					ActualLRPInstanceKey: models.NewActualLRPInstanceKey(instanceGuid, "cell-id"),
					ActualLRPNetInfo:     models.NewActualLRPNetInfo(lrpHost, containerIp, models.NewPortMapping(1234, containerPort)),
					State:                models.ActualLRPStateRunning,
				},
			},
			{
				Instance: &models.ActualLRP{
					ActualLRPKey: models.NewActualLRPKey("", 1, ""),
					State:        models.ActualLRPStateUnclaimed,
				},
			},
		}

		bbsClient.DesiredLRPSchedulingInfosReturns([]*models.DesiredLRPSchedulingInfo{schedulingInfoResponse}, nil)
		bbsClient.ActualLRPGroupsReturns(actualResponses, nil)
	})

	JustBeforeEach(func() {
		logger := lagertest.NewTestLogger("test")
		syncerRunner = syncer.NewSyncer(clock, syncInterval, natsClient, waitForInternalRoutesGreeting, logger)

		shutdown = make(chan struct{})

		process = ifrit.Invoke(syncerRunner)
	})

	AfterEach(func() {
		process.Signal(os.Interrupt)
		Eventually(process.Wait()).Should(Receive(BeNil()))
		close(shutdown)
		close(routerStartMessages)
		close(serviceDiscoveryStartMessages)
	})

	Describe("getting the heartbeat interval from the router", func() {
		var greetings chan *nats.Msg
		BeforeEach(func() {
			greetings = make(chan *nats.Msg, 3)
			natsClient.WhenPublishing("router.greet", func(msg *nats.Msg) error {
				greetings <- msg
				return nil
			})
		})

		Context("when the router emits a router.start", func() {
			Context("using a one second interval", func() {
				JustBeforeEach(func() {
					routerStartMessages <- &nats.Msg{
						Data: []byte(`{
						"minimumRegisterIntervalInSeconds":1,
						"pruneThresholdInSeconds": 3
						}`),
					}
				})

				It("should emit routes with the frequency of the passed-in-interval", func() {
					Eventually(syncerRunner.Events().Sync).Should(Receive())

					clock.WaitForNWatchersAndIncrement(time.Second, 2)
					Eventually(syncerRunner.Events().Emit).Should(Receive())

					clock.WaitForNWatchersAndIncrement(time.Second, 2)
					Eventually(syncerRunner.Events().Emit).Should(Receive())
				})

				It("should only greet the router once", func() {
					Eventually(greetings).Should(Receive())
					Consistently(greetings, 1).ShouldNot(Receive())
				})
			})
		})

		Context("when the router does not emit a router.start", func() {
			It("should keep greeting the router until it gets an interval", func() {
				//get the first greeting
				Eventually(greetings).Should(Receive())

				//get the second greeting, and respond
				clock.WaitForWatcherAndIncrement(time.Second)
				var msg *nats.Msg
				Eventually(greetings).Should(Receive(&msg))
				go natsClient.Publish(msg.Reply, []byte(`{"minimumRegisterIntervalInSeconds":1, "pruneThresholdInSeconds": 3}`))

				//should no longer be greeting the router
				Consistently(greetings).ShouldNot(Receive())
			})
		})

		Context("after getting the first interval, when a second interval arrives", func() {
			JustBeforeEach(func() {
				routerStartMessages <- &nats.Msg{
					Data: []byte(`{"minimumRegisterIntervalInSeconds":1, "pruneThresholdInSeconds": 3}`),
				}
			})

			It("should modify its update rate", func() {
				routerStartMessages <- &nats.Msg{
					Data: []byte(`{"minimumRegisterIntervalInSeconds":2, "pruneThresholdInSeconds": 6}`),
				}

				// first emit should wait a jitter time in response to the incoming heartbeat interval
				Consistently(syncerRunner.Events().Emit).ShouldNot(Receive())
				clock.Increment(400 * time.Millisecond)
				Eventually(syncerRunner.Events().Emit).Should(Receive())

				clock.WaitForWatcherAndIncrement(time.Second)
				Consistently(syncerRunner.Events().Emit).ShouldNot(Receive())

				//i subsequent emit should follow the interval
				clock.WaitForWatcherAndIncrement(time.Second)
				Eventually(syncerRunner.Events().Emit).Should(Receive())
			})

			Context("using different interval", func() {
				It("jitter respects the interval while sleeping", func() {
					routerStartMessages <- &nats.Msg{
						Data: []byte(`{"minimumRegisterIntervalInSeconds":5, "pruneThresholdInSeconds": 180}`),
					}

					// first emit should wait a jitter time in response to the incoming heartbeat interval
					Consistently(syncerRunner.Events().Emit).ShouldNot(Receive())
					clock.Increment(1 * time.Second)
					Eventually(syncerRunner.Events().Emit).Should(Receive())

					// subsequent emit should follow the interval
					clock.WaitForWatcherAndIncrement(5 * time.Second)
					Eventually(syncerRunner.Events().Emit).Should(Receive())
				})
			})

			It("the jitter should be random", func() {
				// This test uses the fact that the probability of the jitter being
				// less than 100 milliseconds should be 50%.
				// However, this test has a 1/512 chance of failing in the case that
				// 10 samples of the jitter are less than 100ms or all 10 samples are
				// greater than 100ms.
				emitted := []bool{}
				for i := 0; i < 10; i++ {
					routerStartMessages <- &nats.Msg{
						Data: []byte(`{"minimumRegisterIntervalInSeconds":1, "pruneThresholdInSeconds": 180}`),
					}

					Consistently(syncerRunner.Events().Emit).ShouldNot(Receive())
					// Wait 10% of the minimum register interval
					clock.Increment(100 * time.Millisecond)
					select {
					case <-syncerRunner.Events().Emit:
						// Statistically 50% of the jitters should end up here
						emitted = append(emitted, true)
						continue
					case <-time.After(10 * time.Millisecond):
						emitted = append(emitted, false)
					}
					// Wait the last 10% to guarantee that an emit happens
					clock.Increment(100 * time.Millisecond)
					Eventually(syncerRunner.Events().Emit).Should(Receive())
				}
				trueCount := 0
				Expect(emitted).To(HaveLen(10))
				for _, e := range emitted {
					if e {
						trueCount++
					}
				}
				Expect(trueCount).To(BeNumerically(">", 0))
				Expect(trueCount).To(BeNumerically("<", 10))
			})
		})

		Context("if it never hears anything from a router anywhere", func() {
			It("should still be able to shutdown", func() {
				process.Signal(os.Interrupt)
				Eventually(process.Wait()).Should(Receive(BeNil()))
			})
		})
	})

	FDescribe("getting the heartbeat interval from service-discovery", func() {
		var greetings chan *nats.Msg
		BeforeEach(func() {
			greetings = make(chan *nats.Msg, 3)
			natsClient.WhenPublishing("service-discovery.greet", func(msg *nats.Msg) error {
				greetings <- msg
				return nil
			})
			waitForInternalRoutesGreeting = true
		})

		Context("when service-discovery emits a service-discovery.start", func() {
			Context("using a one second interval", func() {
				JustBeforeEach(func() {
					routerStartMessages <- &nats.Msg{
						Data: []byte(`{
						"minimumRegisterIntervalInSeconds":1000,
						"pruneThresholdInSeconds": 3000
						}`),
					}
					serviceDiscoveryStartMessages <- &nats.Msg{
						Data: []byte(`{
										"minimumRegisterIntervalInSeconds":1,
										"pruneThresholdInSeconds": 3
										}`),
					}
				})

				It("should emit internal routes with the frequency of the passed-in-interval", func() {
					Eventually(syncerRunner.Events().InternalSync).Should(Receive())

					clock.WaitForNWatchersAndIncrement(time.Second, 2)
					Eventually(syncerRunner.Events().InternalEmit).Should(Receive())

					// clock.WaitForNWatchersAndIncrement(time.Second, 2)
					// Eventually(syncerRunner.Events().InternalEmit).Should(Receive())
				})

				It("should only greet service-discovery once", func() {
					Eventually(greetings).Should(Receive())
					Consistently(greetings, 1).ShouldNot(Receive())
				})
			})
		})
	})

	Describe("syncing", func() {
		BeforeEach(func() {
			bbsClient.ActualLRPGroupsStub = func(logger lager.Logger, f models.ActualLRPFilter) ([]*models.ActualLRPGroup, error) {
				return nil, nil
			}
			syncInterval = 500 * time.Millisecond
		})

		JustBeforeEach(func() {
			//we set the emit interval real high to avoid colliding with our sync interval
			routerStartMessages <- &nats.Msg{
				Data: []byte(`{"minimumRegisterIntervalInSeconds":10, "pruneThresholdInSeconds": 20}`),
			}
		})

		Context("after the router greets", func() {
			BeforeEach(func() {
				syncInterval = 10 * time.Minute
			})

			It("syncs", func() {
				Eventually(syncerRunner.Events().Sync).Should(Receive())
			})
		})

		Context("on a specified interval", func() {
			It("should sync", func() {
				clock.WaitForWatcherAndIncrement(syncInterval)
				Eventually(syncerRunner.Events().Sync).Should(Receive())

				clock.WaitForWatcherAndIncrement(syncInterval)
				Eventually(syncerRunner.Events().Sync).Should(Receive())
			})
		})
	})
})
