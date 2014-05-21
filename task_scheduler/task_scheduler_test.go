package task_scheduler_test

import (
	"errors"

	"github.com/cloudfoundry-incubator/executor/api"
	"github.com/tedsuo/router"

	"github.com/cloudfoundry-incubator/executor/client/fake_client"
	"github.com/cloudfoundry-incubator/rep/routes"
	"github.com/cloudfoundry-incubator/rep/task_scheduler"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gosteno"
	"github.com/onsi/gomega/ghttp"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("TaskScheduler", func() {
	var logger *gosteno.Logger

	BeforeSuite(func() {
		gosteno.EnterTestMode(gosteno.LOG_DEBUG)
	})

	BeforeEach(func() {
		logger = gosteno.NewLogger("test-logger")
	})

	Context("when a game scheduler is running", func() {
		var fakeExecutor *ghttp.Server
		var fakeBBS *fake_bbs.FakeRepBBS
		var taskScheduler *task_scheduler.TaskScheduler
		var correctStack = "my-stack"
		var fakeClient *fake_client.FakeClient

		BeforeEach(func() {
			fakeClient = fake_client.New()
			fakeExecutor = ghttp.NewServer()
			fakeBBS = fake_bbs.NewFakeRepBBS()

			taskScheduler = task_scheduler.New(
				router.NewRequestGenerator(
					routes.TaskCompleted,
					routes.Routes,
				),
				fakeBBS,
				logger,
				correctStack,
				fakeClient,
			)
		})

		AfterEach(func() {
			taskScheduler.Stop()
			fakeExecutor.Close()
		})

		JustBeforeEach(func() {
			readyChan := make(chan struct{})
			err := taskScheduler.Run(readyChan)
			Ω(err).ShouldNot(HaveOccurred())
			<-readyChan
		})

		Context("when a staging task is desired", func() {
			var task models.Task

			BeforeEach(func() {
				index := 0

				task = models.Task{
					Guid:       "task-guid-123",
					Stack:      correctStack,
					MemoryMB:   64,
					DiskMB:     1024,
					CpuPercent: .5,
					Actions: []models.ExecutorAction{
						{
							Action: models.RunAction{
								Script:  "the-script",
								Env:     []models.EnvironmentVariable{{Key: "PATH", Value: "the-path"}},
								Timeout: 500,
							},
						},
					},
					Log: models.LogConfig{
						Guid:       "some-guid",
						SourceName: "XYZ",
						Index:      &index,
					},
				}

				fakeBBS.EmitDesiredTask(task)
			})

			Context("when reserving the container succeeds", func() {
				var allocateCalled chan struct{}
				var deletedContainerGuid chan string

				BeforeEach(func() {
					allocateCalled = make(chan struct{}, 1)
					deletedContainerGuid = make(chan string, 1)

					fakeClient.WhenAllocatingContainer = func(containerGuid string, req api.ContainerAllocationRequest) (api.Container, error) {
						defer GinkgoRecover()

						allocateCalled <- struct{}{}
						Ω(fakeBBS.ClaimedTasks()).Should(HaveLen(0))
						Ω(req.MemoryMB).Should(Equal(64))
						Ω(req.DiskMB).Should(Equal(1024))
						Ω(req.CpuPercent).Should(Equal(0.5))
						Ω(req.Log).Should(Equal(task.Log))
						Ω(containerGuid).Should(Equal(task.Guid))
						return api.Container{ExecutorGuid: "the-executor-guid", Guid: containerGuid}, nil
					}

					fakeClient.WhenDeletingContainer = func(allocationGuid string) error {
						deletedContainerGuid <- allocationGuid
						return nil
					}
				})

				Context("when claiming the task succeeds", func() {
					Context("when initializing the container succeeds", func() {
						var initCalled chan struct{}

						BeforeEach(func() {
							initCalled = make(chan struct{}, 1)

							fakeClient.WhenInitializingContainer = func(allocationGuid string) error {
								defer GinkgoRecover()

								initCalled <- struct{}{}
								Ω(allocationGuid).Should(Equal(task.Guid))
								Ω(fakeBBS.ClaimedTasks()).Should(HaveLen(1))
								Ω(fakeBBS.StartedTasks()).Should(HaveLen(0))
								return nil
							}
						})

						Context("and the executor successfully starts running the task", func() {
							var (
								reqChan chan api.ContainerRunRequest
							)

							BeforeEach(func() {
								reqChan = make(chan api.ContainerRunRequest, 1)

								fakeClient.WhenRunning = func(allocationGuid string, req api.ContainerRunRequest) error {
									defer GinkgoRecover()

									Ω(fakeBBS.StartedTasks()).Should(HaveLen(1))

									expectedTask := task
									expectedTask.ExecutorID = "the-executor-guid"
									expectedTask.ContainerHandle = allocationGuid
									Ω(fakeBBS.StartedTasks()[0]).Should(Equal(expectedTask))

									Ω(allocationGuid).Should(Equal(task.Guid))
									Ω(req.Actions).Should(Equal(task.Actions))

									reqChan <- req
									return nil
								}
							})

							It("makes all calls to the executor", func() {
								Eventually(allocateCalled).Should(Receive())
								Eventually(initCalled).Should(Receive())
								Eventually(reqChan).Should(Receive())
							})
						})

						Context("but starting the task fails", func() {
							BeforeEach(func() {
								fakeBBS.SetStartTaskErr(errors.New("kerpow"))
							})

							It("deletes the container", func() {
								Eventually(deletedContainerGuid).Should(Receive(Equal(task.Guid)))
							})
						})
					})

					Context("but initializing the container fails", func() {
						BeforeEach(func() {
							fakeClient.WhenInitializingContainer = func(allocationGuid string) error {
								return errors.New("Can't initialize")
							}
						})

						It("does not mark the job as started", func() {
							Eventually(fakeBBS.StartedTasks).Should(HaveLen(0))
						})

						It("deletes the container", func() {
							Eventually(deletedContainerGuid).Should(Receive(Equal(task.Guid)))
						})
					})
				})

				Context("but claiming the task fails", func() {
					BeforeEach(func() {
						fakeBBS.SetClaimTaskErr(errors.New("data store went away."))
					})

					It("deletes the resource allocation on the executor", func() {
						Eventually(deletedContainerGuid).Should(Receive(Equal(task.Guid)))
					})
				})
			})

			Context("when reserving the container fails", func() {
				var allocatedContainer chan struct{}

				BeforeEach(func() {
					allocatedContainer = make(chan struct{}, 1)

					fakeClient.WhenAllocatingContainer = func(guid string, req api.ContainerAllocationRequest) (api.Container, error) {
						allocatedContainer <- struct{}{}
						return api.Container{}, errors.New("Something went wrong")
					}
				})

				It("makes the resource allocation request", func() {
					Eventually(allocatedContainer).Should(Receive())
				})

				It("does not mark the job as Claimed", func() {
					Eventually(fakeBBS.ClaimedTasks).Should(HaveLen(0))
				})

				It("does not mark the job as Started", func() {
					Eventually(fakeBBS.StartedTasks).Should(HaveLen(0))
				})
			})
		})

		Context("when the task has the wrong stack", func() {
			var task models.Task

			BeforeEach(func() {
				task = models.Task{
					Guid:       "task-guid-123",
					Stack:      "asd;oubhasdfbuvasfb",
					MemoryMB:   64,
					DiskMB:     1024,
					CpuPercent: .5,
					Actions:    []models.ExecutorAction{},
				}
				fakeBBS.EmitDesiredTask(task)
			})

			It("ignores the task", func() {
				Consistently(fakeBBS.ClaimedTasks).Should(BeEmpty())
			})
		})
	})
})