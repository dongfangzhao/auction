package auctionrunner_test

import (
	"time"

	"github.com/cloudfoundry/gunk/workpool"
	"github.com/pivotal-golang/clock/fakeclock"

	"github.com/cloudfoundry-incubator/auction/auctionrunner"
	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/cloudfoundry-incubator/auction/auctiontypes/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/diego_errors"
	"github.com/cloudfoundry-incubator/runtime-schema/models"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Scheduler", func() {
	var clients map[string]*fakes.FakeSimulationCellRep
	var zones map[string]auctionrunner.Zone
	var clock *fakeclock.FakeClock
	var workPool *workpool.WorkPool
	var results auctiontypes.AuctionResults

	BeforeEach(func() {
		clock = fakeclock.NewFakeClock(time.Now())
		workPool = workpool.NewWorkPool(5)

		clients = map[string]*fakes.FakeSimulationCellRep{}
		zones = map[string]auctionrunner.Zone{}
	})

	AfterEach(func() {
		workPool.Stop()
	})

	Context("when the cells are empty", func() {
		It("immediately returns everything as having failed, incrementing the attempt number", func() {
			startAuction := BuildLRPAuction("pg-7", 0, lucidRootFSURL, 10, 10, clock.Now())

			taskAuction := BuildTaskAuction(BuildTask("tg-1", lucidRootFSURL, 0, 0), clock.Now())

			auctionRequest := auctiontypes.AuctionRequest{
				LRPs:  []auctiontypes.LRPAuction{startAuction},
				Tasks: []auctiontypes.TaskAuction{taskAuction},
			}

			By("no auctions are marked successful")
			scheduler := auctionrunner.NewScheduler(workPool, map[string]auctionrunner.Zone{}, clock)
			results := scheduler.Schedule(auctionRequest)
			Expect(results.SuccessfulLRPs).To(BeEmpty())
			Expect(results.SuccessfulTasks).To(BeEmpty())

			By("all lrp starts are marked failed, and their attempts are incremented")
			Expect(results.FailedLRPs).To(HaveLen(1))
			failedLRPStart := results.FailedLRPs[0]
			Expect(failedLRPStart.Identifier()).To(Equal(startAuction.Identifier()))
			Expect(failedLRPStart.Attempts).To(Equal(startAuction.Attempts + 1))
			Expect(failedLRPStart.PlacementError).To(Equal(diego_errors.CELL_MISMATCH_MESSAGE))

			By("all tasks are marked failed, and their attempts are incremented")
			Expect(results.FailedTasks).To(HaveLen(1))
			failedTask := results.FailedTasks[0]
			Expect(failedTask.Identifier()).To(Equal(taskAuction.Identifier()))
			Expect(failedTask.Attempts).To(Equal(taskAuction.Attempts + 1))
			Expect(failedTask.PlacementError).To(Equal(diego_errors.CELL_MISMATCH_MESSAGE))
		})

	})

	Describe("handling start auctions", func() {
		var startAuction auctiontypes.LRPAuction

		BeforeEach(func() {
			clients["A-cell"] = &fakes.FakeSimulationCellRep{}
			zones["A-zone"] = auctionrunner.Zone{
				auctionrunner.NewCell(
					"A-cell",
					clients["A-cell"],
					BuildCellState("A-zone", 100, 100, 100, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{
						{"pg-1", 0, 10, 10},
						{"pg-2", 0, 10, 10},
					}),
				),
			}

			clients["B-cell"] = &fakes.FakeSimulationCellRep{}
			zones["B-zone"] = auctionrunner.Zone{
				auctionrunner.NewCell(
					"B-cell",
					clients["B-cell"],
					BuildCellState("B-zone", 100, 100, 100, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{
						{"pg-3", 0, 10, 10},
					}),
				),
			}
		})

		Context("when only one of many zones supports a specific RootFS", func() {
			BeforeEach(func() {
				clients["C-cell"] = &fakes.FakeSimulationCellRep{}
				zones["C-zone"] = auctionrunner.Zone{
					auctionrunner.NewCell(
						"C-cell",
						clients["C-cell"],
						BuildCellState("C-zone", 100, 100, 100, false, windowsOnlyRootFSProviders, []auctiontypes.LRP{
							{"pg-win-1", 0, 10, 10},
						}),
					),
				}
			})

			Context("with a new LRP only supported in one of many zones", func() {
				BeforeEach(func() {
					startAuction = BuildLRPAuction("pg-win-2", 1, windowsRootFSURL, 10, 10, clock.Now())
				})

				Context("when it picks a winner", func() {
					BeforeEach(func() {
						clock.Increment(time.Minute)
						s := auctionrunner.NewScheduler(workPool, zones, clock)
						results = s.Schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{startAuction}})
					})

					It("picks the best cell for the job", func() {
						Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
						Expect(clients["B-cell"].PerformCallCount()).To(Equal(0))
						Expect(clients["C-cell"].PerformCallCount()).To(Equal(1))

						startsToC := clients["C-cell"].PerformArgsForCall(0).LRPs

						Expect(startsToC).To(ConsistOf(startAuction))
					})

					It("marks the start auction as succeeded", func() {
						setLRPWinner("C-cell", &startAuction)
						startAuction.WaitDuration = time.Minute
						Expect(results.SuccessfulLRPs).To(ConsistOf(startAuction))
						Expect(results.FailedLRPs).To(BeEmpty())
					})
				})
			})
		})

		Context("with an existing LRP (zone balancing)", func() {
			BeforeEach(func() {
				startAuction = BuildLRPAuction("pg-3", 1, lucidRootFSURL, 10, 10, clock.Now())
			})

			Context("when it picks a winner", func() {
				BeforeEach(func() {
					clock.Increment(time.Minute)

					s := auctionrunner.NewScheduler(workPool, zones, clock)
					results = s.Schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{startAuction}})
				})

				It("picks the best cell for the job", func() {
					Expect(clients["A-cell"].PerformCallCount()).To(Equal(1))
					Expect(clients["B-cell"].PerformCallCount()).To(Equal(0))

					startsToA := clients["A-cell"].PerformArgsForCall(0).LRPs

					Expect(startsToA).To(ConsistOf(startAuction))
				})

				It("marks the start auction as succeeded", func() {
					setLRPWinner("A-cell", &startAuction)
					startAuction.WaitDuration = time.Minute
					Expect(results.SuccessfulLRPs).To(ConsistOf(startAuction))
					Expect(results.FailedLRPs).To(BeEmpty())
				})
			})
		})

		Context("with a new LRP (cell balancing)", func() {
			BeforeEach(func() {
				startAuction = BuildLRPAuction("pg-4", 1, lucidRootFSURL, 10, 10, clock.Now())
			})

			Context("when it picks a winner", func() {
				BeforeEach(func() {
					clock.Increment(time.Minute)
					s := auctionrunner.NewScheduler(workPool, zones, clock)
					results = s.Schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{startAuction}})
				})

				It("picks the best cell for the job", func() {
					Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
					Expect(clients["B-cell"].PerformCallCount()).To(Equal(1))

					startsToB := clients["B-cell"].PerformArgsForCall(0).LRPs

					Expect(startsToB).To(ConsistOf(startAuction))
				})

				It("marks the start auction as succeeded", func() {
					setLRPWinner("B-cell", &startAuction)
					startAuction.WaitDuration = time.Minute
					Expect(results.SuccessfulLRPs).To(ConsistOf(startAuction))
					Expect(results.FailedLRPs).To(BeEmpty())
				})
			})
		})

		Context("when the cell rejects the start auction", func() {
			BeforeEach(func() {
				startAuction = BuildLRPAuction("pg-3", 1, lucidRootFSURL, 10, 10, clock.Now())

				clients["A-cell"].PerformReturns(auctiontypes.Work{LRPs: []auctiontypes.LRPAuction{startAuction}}, nil)
				clients["B-cell"].PerformReturns(auctiontypes.Work{LRPs: []auctiontypes.LRPAuction{startAuction}}, nil)

				clock.Increment(time.Minute)
				s := auctionrunner.NewScheduler(workPool, zones, clock)
				results = s.Schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{startAuction}})
			})

			It("marks the start auction as failed", func() {
				startAuction.Attempts = 1
				Expect(results.SuccessfulLRPs).To(BeEmpty())
				Expect(results.FailedLRPs).To(ConsistOf(startAuction))
			})
		})

		Context("when there is no room", func() {
			BeforeEach(func() {
				startAuction = BuildLRPAuctionWithPlacementError("pg-4", 0, lucidRootFSURL, 1000, 1000, clock.Now(), diego_errors.INSUFFICIENT_RESOURCES_MESSAGE)
				clock.Increment(time.Minute)
				s := auctionrunner.NewScheduler(workPool, zones, clock)
				results = s.Schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{startAuction}})
			})

			It("should not attempt to start the LRP", func() {
				Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
				Expect(clients["B-cell"].PerformCallCount()).To(Equal(0))
			})

			It("should mark the start auction as failed", func() {
				startAuction.Attempts = 1
				Expect(results.SuccessfulLRPs).To(BeEmpty())
				Expect(results.FailedLRPs).To(ConsistOf(startAuction))
			})
		})
	})

	Describe("handling task auctions", func() {
		var taskAuction auctiontypes.TaskAuction

		BeforeEach(func() {
			clients["A-cell"] = &fakes.FakeSimulationCellRep{}
			zones["A-zone"] = auctionrunner.Zone{auctionrunner.NewCell("A-cell", clients["A-cell"], BuildCellState("A-zone", 100, 100, 100, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{
				{"does-not-matter", 0, 10, 10},
				{"does-not-matter", 0, 10, 10},
			}))}

			clients["B-cell"] = &fakes.FakeSimulationCellRep{}
			zones["B-zone"] = auctionrunner.Zone{auctionrunner.NewCell("B-cell", clients["B-cell"], BuildCellState("B-zone", 100, 100, 100, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{
				{"does-not-matter", 0, 10, 10},
			}))}

			taskAuction = BuildTaskAuction(BuildTask("tg-1", lucidRootFSURL, 10, 10), clock.Now())
			clock.Increment(time.Minute)
		})

		Context("when only one of many zones supports a specific RootFS", func() {
			BeforeEach(func() {
				clients["C-cell"] = &fakes.FakeSimulationCellRep{}
				zones["C-zone"] = auctionrunner.Zone{
					auctionrunner.NewCell(
						"C-cell",
						clients["C-cell"],
						BuildCellState("C-zone", 100, 100, 100, false, windowsOnlyRootFSProviders, []auctiontypes.LRP{
							{"tg-win-1", 0, 10, 10},
						}),
					),
				}
			})

			Context("with a new Task only supported in one of many zones", func() {
				BeforeEach(func() {
					taskAuction = BuildTaskAuction(BuildTask("tg-win-2", windowsRootFSURL, 10, 10), clock.Now())
				})

				Context("when it picks a winner", func() {
					BeforeEach(func() {
						clock.Increment(time.Minute)
						s := auctionrunner.NewScheduler(workPool, zones, clock)
						results = s.Schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{taskAuction}})
					})

					It("picks the best cell for the job", func() {
						Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
						Expect(clients["B-cell"].PerformCallCount()).To(Equal(0))
						Expect(clients["C-cell"].PerformCallCount()).To(Equal(1))

						startsToC := clients["C-cell"].PerformArgsForCall(0).Tasks

						Expect(startsToC).To(ConsistOf(taskAuction.Task))
					})

					It("marks the start auction as succeeded", func() {
						setTaskWinner("C-cell", &taskAuction)
						taskAuction.WaitDuration = time.Minute
						Expect(results.SuccessfulTasks).To(ConsistOf(taskAuction))
						Expect(results.FailedTasks).To(BeEmpty())
					})
				})
			})
		})

		Context("when it picks a winner", func() {
			BeforeEach(func() {
				s := auctionrunner.NewScheduler(workPool, zones, clock)
				results = s.Schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{taskAuction}})
			})

			It("picks the best cell for the job", func() {
				Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
				Expect(clients["B-cell"].PerformCallCount()).To(Equal(1))

				tasksToB := clients["B-cell"].PerformArgsForCall(0).Tasks

				Expect(tasksToB).To(ConsistOf(
					taskAuction.Task,
				))

			})

			It("marks the task auction as succeeded", func() {
				Expect(results.SuccessfulTasks).To(HaveLen(1))
				successfulTask := results.SuccessfulTasks[0]
				Expect(successfulTask.Winner).To(Equal("B-cell"))
				Expect(successfulTask.Attempts).To(Equal(1))
				Expect(successfulTask.WaitDuration).To(Equal(time.Minute))

				Expect(results.FailedTasks).To(BeEmpty())
			})
		})

		Context("when the cell rejects the task", func() {
			BeforeEach(func() {
				clients["B-cell"].PerformReturns(auctiontypes.Work{Tasks: []models.Task{taskAuction.Task}}, nil)
				s := auctionrunner.NewScheduler(workPool, zones, clock)
				results = s.Schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{taskAuction}})
			})

			It("marks the task auction as failed", func() {
				Expect(results.SuccessfulTasks).To(BeEmpty())

				Expect(results.FailedTasks).To(HaveLen(1))
				failedTask := results.FailedTasks[0]
				Expect(failedTask.Attempts).To(Equal(1))
			})
		})

		Context("when there is no room", func() {
			BeforeEach(func() {
				taskAuction = BuildTaskAuction(BuildTask("tg-1", lucidRootFSURL, 1000, 1000), clock.Now())
				clock.Increment(time.Minute)
				s := auctionrunner.NewScheduler(workPool, zones, clock)
				results = s.Schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{taskAuction}})
			})

			It("should not attempt to start the task", func() {
				Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
				Expect(clients["B-cell"].PerformCallCount()).To(Equal(0))
			})

			It("should mark the start auction as failed", func() {
				Expect(results.SuccessfulTasks).To(BeEmpty())

				Expect(results.FailedTasks).To(HaveLen(1))
				failedTask := results.FailedTasks[0]
				Expect(failedTask.Attempts).To(Equal(1))
				Expect(failedTask.PlacementError).To(Equal(diego_errors.INSUFFICIENT_RESOURCES_MESSAGE))
			})
		})

		Context("when there is cell mismatch", func() {
			BeforeEach(func() {
				taskAuction = BuildTaskAuction(BuildTask("tg-1", "unsupported:rootfs", 100, 100), clock.Now())
				clock.Increment(time.Minute)
				s := auctionrunner.NewScheduler(workPool, zones, clock)
				results = s.Schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{taskAuction}})
			})

			It("should not attempt to start the task", func() {
				Expect(clients["A-cell"].PerformCallCount()).To(Equal(0))
				Expect(clients["B-cell"].PerformCallCount()).To(Equal(0))
			})

			It("should mark the start auction as failed", func() {
				Expect(results.SuccessfulTasks).To(BeEmpty())

				Expect(results.FailedTasks).To(HaveLen(1))
				failedTask := results.FailedTasks[0]
				Expect(failedTask.Attempts).To(Equal(1))
				Expect(failedTask.PlacementError).To(Equal(diego_errors.CELL_MISMATCH_MESSAGE))
			})
		})
	})

	Describe("a comprehensive scenario", func() {
		BeforeEach(func() {
			clients["A-cell"] = &fakes.FakeSimulationCellRep{}
			zones["A-zone"] = auctionrunner.Zone{auctionrunner.NewCell("A-cell", clients["A-cell"], BuildCellState("A-zone", 100, 100, 100, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{
				{"pg-1", 0, 10, 10},
				{"pg-2", 0, 10, 10},
			}))}

			clients["B-cell"] = &fakes.FakeSimulationCellRep{}
			zones["B-zone"] = auctionrunner.Zone{auctionrunner.NewCell("B-cell", clients["B-cell"], BuildCellState("B-zone", 100, 100, 100, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{
				{"pg-3", 0, 10, 10},
				{"pg-4", 0, 20, 20},
			}))}
		})

		It("should optimize the distribution", func() {
			startPG3 := BuildLRPAuction(
				"pg-3", 1, lucidRootFSURL, 40, 40,
				clock.Now(),
			)
			startPG2 := BuildLRPAuction(
				"pg-2", 1, lucidRootFSURL, 5, 5,
				clock.Now(),
			)
			startPGNope := BuildLRPAuctionWithPlacementError(
				"pg-nope", 1, ".net", 10, 10,
				clock.Now(),
				diego_errors.CELL_MISMATCH_MESSAGE,
			)

			taskAuction1 := BuildTaskAuction(
				BuildTask("tg-1", lucidRootFSURL, 40, 40),
				clock.Now(),
			)
			taskAuction2 := BuildTaskAuction(
				BuildTask("tg-2", lucidRootFSURL, 5, 5),
				clock.Now(),
			)
			taskAuctionNope := BuildTaskAuction(
				BuildTask("tg-nope", ".net", 1, 1),
				clock.Now(),
			)

			auctionRequest := auctiontypes.AuctionRequest{
				LRPs:  []auctiontypes.LRPAuction{startPG3, startPG2, startPGNope},
				Tasks: []auctiontypes.TaskAuction{taskAuction1, taskAuction2, taskAuctionNope},
			}

			s := auctionrunner.NewScheduler(workPool, zones, clock)
			results = s.Schedule(auctionRequest)

			Expect(clients["A-cell"].PerformCallCount()).To(Equal(1))
			Expect(clients["B-cell"].PerformCallCount()).To(Equal(1))

			Expect(clients["A-cell"].PerformArgsForCall(0).LRPs).To(ConsistOf(startPG3))
			Expect(clients["B-cell"].PerformArgsForCall(0).LRPs).To(ConsistOf(startPG2))

			Expect(clients["A-cell"].PerformArgsForCall(0).Tasks).To(ConsistOf(taskAuction1.Task))
			Expect(clients["B-cell"].PerformArgsForCall(0).Tasks).To(ConsistOf(taskAuction2.Task))

			setLRPWinner("A-cell", &startPG3)
			setLRPWinner("B-cell", &startPG2)
			Expect(results.SuccessfulLRPs).To(ConsistOf(startPG3, startPG2))

			Expect(results.SuccessfulTasks).To(HaveLen(2))
			var successfulTaskAuction1, successfulTaskAuction2 auctiontypes.TaskAuction
			for _, ta := range results.SuccessfulTasks {
				if ta.Identifier() == taskAuction1.Identifier() {
					successfulTaskAuction1 = ta
				} else if ta.Identifier() == taskAuction2.Identifier() {
					successfulTaskAuction2 = ta
				}
			}
			Expect(successfulTaskAuction1).NotTo(BeNil())
			Expect(successfulTaskAuction1.Attempts).To(Equal(1))
			Expect(successfulTaskAuction1.Winner).To(Equal("A-cell"))
			Expect(successfulTaskAuction2).NotTo(BeNil())
			Expect(successfulTaskAuction2.Attempts).To(Equal(1))
			Expect(successfulTaskAuction2.Winner).To(Equal("B-cell"))

			startPGNope.Attempts = 1
			Expect(results.FailedLRPs).To(ConsistOf(startPGNope))
			Expect(results.FailedTasks).To(HaveLen(1))

			failedTask := results.FailedTasks[0]
			Expect(failedTask.Identifier()).To(Equal(taskAuctionNope.Identifier()))
			Expect(failedTask.Attempts).To(Equal(1))
		})
	})

	Describe("ordering work", func() {
		var (
			pg70, pg71, pg81, pg82 auctiontypes.LRPAuction
			tg1, tg2               auctiontypes.TaskAuction
			memory                 int

			lrps  []auctiontypes.LRPAuction
			tasks []auctiontypes.TaskAuction
		)

		BeforeEach(func() {
			clients["cell"] = &fakes.FakeSimulationCellRep{}

			pg70 = BuildLRPAuction("pg-7", 0, lucidRootFSURL, 10, 10, clock.Now())
			pg71 = BuildLRPAuction("pg-7", 1, lucidRootFSURL, 10, 10, clock.Now())
			pg81 = BuildLRPAuction("pg-8", 1, lucidRootFSURL, 40, 40, clock.Now())
			pg82 = BuildLRPAuction("pg-8", 2, lucidRootFSURL, 40, 40, clock.Now())
			lrps = []auctiontypes.LRPAuction{pg70, pg71, pg81, pg82}

			tg1 = BuildTaskAuction(BuildTask("tg-1", lucidRootFSURL, 10, 10), clock.Now())
			tg2 = BuildTaskAuction(BuildTask("tg-2", lucidRootFSURL, 20, 20), clock.Now())
			tasks = []auctiontypes.TaskAuction{tg1, tg2}

			memory = 100
		})

		JustBeforeEach(func() {
			zones["zone"] = auctionrunner.Zone{
				auctionrunner.NewCell("cell", clients["cell"], BuildCellState("zone", memory, 1000, 1000, false, lucidOnlyRootFSProviders, []auctiontypes.LRP{})),
			}

			auctionRequest := auctiontypes.AuctionRequest{
				LRPs:  lrps,
				Tasks: tasks,
			}

			scheduler := auctionrunner.NewScheduler(workPool, zones, clock)
			results = scheduler.Schedule(auctionRequest)
		})

		Context("where there are sufficient resources", func() {
			BeforeEach(func() {
				memory = 130
			})

			It("schedules all LPRs and tasks", func() {
				setLRPWinner("cell", &pg70, &pg71, &pg81, &pg82)
				setTaskWinner("cell", &tg1, &tg2)

				Expect(results.SuccessfulLRPs).To(ConsistOf(pg70, pg71, pg81, pg82))
				Expect(results.SuccessfulTasks).To(ConsistOf(tg1, tg2))
			})
		})

		Context("when there are insufficient resources", func() {
			BeforeEach(func() {
				memory = 10
			})

			It("schedules LRP instances with index 0 first", func() {
				setLRPWinner("cell", &pg70)

				Expect(results.SuccessfulLRPs).To(ConsistOf(pg70))
				Expect(results.SuccessfulTasks).To(BeEmpty())
			})

			Context("with just a bit more resources", func() {
				BeforeEach(func() {
					memory = 45
				})

				It("schedules tasks before LRP instances with index > 0", func() {
					setLRPWinner("cell", &pg70)
					setTaskWinner("cell", &tg1, &tg2)

					Expect(results.SuccessfulLRPs).To(ConsistOf(pg70))
					Expect(results.SuccessfulTasks).To(ConsistOf(tg1, tg2))
				})

				Context("with even more resources", func() {
					BeforeEach(func() {
						memory = 95
					})

					It("schedules LRPs with index > 0 after tasks, by index", func() {
						setLRPWinner("cell", &pg70, &pg71, &pg81)
						setTaskWinner("cell", &tg1, &tg2)

						Expect(results.SuccessfulLRPs).To(ConsistOf(pg70, pg71, pg81))
						Expect(results.SuccessfulTasks).To(ConsistOf(tg1, tg2))
					})
				})
			})
		})

		Context("when LRP indices match", func() {
			BeforeEach(func() {
				memory = 80
			})

			It("schedules boulders before pebbles", func() {
				setLRPWinner("cell", &pg70, &pg81)
				setTaskWinner("cell", &tg1, &tg2)

				Expect(results.SuccessfulLRPs).To(ConsistOf(pg70, pg81))
				Expect(results.SuccessfulTasks).To(ConsistOf(tg1, tg2))
			})
		})

		Context("when dealing with tasks", func() {
			var tg3 auctiontypes.TaskAuction

			BeforeEach(func() {
				tg3 = BuildTaskAuction(BuildTask("tg-3", lucidRootFSURL, 30, 30), clock.Now())
				lrps = []auctiontypes.LRPAuction{}
				tasks = append(tasks, tg3)
				memory = tg3.Task.MemoryMB + 1
			})

			It("schedules boulders before pebbles", func() {
				setTaskWinner("cell", &tg3)
				Expect(results.SuccessfulTasks).To(ConsistOf(tg3))
			})
		})
	})
})

func setLRPWinner(cellName string, lrps ...*auctiontypes.LRPAuction) {
	for _, l := range lrps {
		l.Winner = cellName
		l.Attempts++
	}
}

func setTaskWinner(cellName string, tasks ...*auctiontypes.TaskAuction) {
	for _, t := range tasks {
		t.Winner = cellName
		t.Attempts++
	}
}
