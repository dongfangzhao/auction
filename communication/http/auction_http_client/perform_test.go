package auction_http_client_test

import (
	"errors"

	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Perform", func() {
	var work, failedWork auctiontypes.Work

	BeforeEach(func() {
		work = auctiontypes.Work{
			Tasks: []models.Task{
				{
					TaskGuid: "tg-a",
				},
				{
					TaskGuid: "tg-b",
				},
			},
		}

		failedWork = auctiontypes.Work{
			Tasks: []models.Task{
				{
					TaskGuid: "pg-a",
				},
			},
		}
	})

	It("should tell the rep to perform", func() {
		Expect(auctionRep.PerformCallCount()).To(Equal(0))
		client.Perform(work)
		Expect(auctionRep.PerformArgsForCall(0)).To(Equal(work))
	})

	Context("when the request succeeds", func() {
		BeforeEach(func() {
			auctionRep.PerformReturns(failedWork, nil)
		})

		It("should return the state returned by the rep", func() {
			Expect(client.Perform(work)).To(Equal(failedWork))
		})
	})

	Context("when the request fails", func() {
		BeforeEach(func() {
			auctionRep.PerformReturns(failedWork, errors.New("boom"))
		})

		It("should error", func() {
			failedWork, err := client.Perform(work)
			Expect(failedWork).To(BeZero())
			Expect(err).To(HaveOccurred())
		})
	})

	Context("when a request errors (in the network sense)", func() {
		It("should error", func() {
			failedWork, err := clientForServerThatErrors.Perform(work)
			Expect(failedWork).To(BeZero())
			Expect(err).To(HaveOccurred())
		})
	})
})
