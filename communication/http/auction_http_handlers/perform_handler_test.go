package auction_http_handlers_test

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/cloudfoundry-incubator/auction/communication/http/routes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Perform", func() {
	Context("with valid JSON", func() {
		var requestedWork, failedWork auctiontypes.Work
		BeforeEach(func() {
			requestedWork = auctiontypes.Work{
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

		Context("and no perform error", func() {
			BeforeEach(func() {
				auctionRep.PerformReturns(failedWork, nil)
			})

			It("succeeds, returning any failed work", func() {
				Expect(auctionRep.PerformCallCount()).To(Equal(0))

				status, body := Request(routes.Perform, nil, JSONReaderFor(requestedWork))
				Expect(status).To(Equal(http.StatusOK))
				Expect(body).To(MatchJSON(JSONFor(failedWork)))

				Expect(auctionRep.PerformCallCount()).To(Equal(1))
				Expect(auctionRep.PerformArgsForCall(0)).To(Equal(requestedWork))
			})
		})

		Context("and a perform error", func() {
			BeforeEach(func() {
				auctionRep.PerformReturns(failedWork, errors.New("kaboom"))
			})

			It("fails, returning nothing", func() {
				Expect(auctionRep.PerformCallCount()).To(Equal(0))

				status, body := Request(routes.Perform, nil, JSONReaderFor(requestedWork))
				Expect(status).To(Equal(http.StatusInternalServerError))
				Expect(body).To(BeEmpty())

				Expect(auctionRep.PerformCallCount()).To(Equal(1))
				Expect(auctionRep.PerformArgsForCall(0)).To(Equal(requestedWork))
			})
		})
	})

	Context("with invalid JSON", func() {
		It("fails", func() {
			Expect(auctionRep.PerformCallCount()).To(Equal(0))

			status, body := Request(routes.Perform, nil, bytes.NewBufferString("∆"))
			Expect(status).To(Equal(http.StatusBadRequest))
			Expect(body).To(BeEmpty())

			Expect(auctionRep.PerformCallCount()).To(Equal(0))
		})
	})
})
