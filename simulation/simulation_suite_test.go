package simulation_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/cloudfoundry-incubator/auction/communication/http/auction_http_client"

	"github.com/cloudfoundry-incubator/auction/auctionrep"
	"github.com/cloudfoundry-incubator/auction/auctionrunner"
	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/cloudfoundry-incubator/auction/communication/nats/auction_nats_client"
	"github.com/cloudfoundry-incubator/auction/simulation/auctiondistributor"
	"github.com/cloudfoundry-incubator/auction/simulation/communication/inprocess"
	"github.com/cloudfoundry-incubator/auction/simulation/simulationrepdelegate"
	"github.com/cloudfoundry-incubator/auction/simulation/visualization"
	"github.com/cloudfoundry-incubator/auction/util"
	"github.com/cloudfoundry/gunk/natsrunner"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/pivotal-golang/lager"

	"testing"
	"time"
)

var communicationMode string
var auctioneerMode string

const InProcess = "inprocess"
const NATS = "nats"
const HTTP = "http"
const DiegoNATSCommunicationMode = "diego-nats"
const DiegoHTTPCommunicationMode = "diego-http"
const ExternalAuctioneerMode = "external"

const numAuctioneers = 100
const numReps = 100
const LatencyMin = 1 * time.Millisecond
const LatencyMax = 2 * time.Millisecond

var repResources = auctiontypes.Resources{
	MemoryMB:   100.0,
	DiskMB:     100.0,
	Containers: 100,
}

var maxConcurrentPerExecutor int

var timeout time.Duration
var auctionDistributor auctiondistributor.AuctionDistributor

var svgReport *visualization.SVGReport
var reports []*visualization.Report

var sessionsToTerminate []*gexec.Session
var natsRunner *natsrunner.NATSRunner
var client auctiontypes.SimulationRepPoolClient
var repAddresses []auctiontypes.RepAddress
var reportName string

var disableSVGReport bool

func init() {
	flag.StringVar(&communicationMode, "communicationMode", "inprocess", "one of inprocess, nats, http, diego-nats or diego-http")
	flag.StringVar(&auctioneerMode, "auctioneerMode", "inprocess", "one of inprocess or external")
	flag.DurationVar(&timeout, "timeout", time.Second, "timeout when waiting for responses from remote calls")

	flag.StringVar(&(auctionrunner.DefaultStartAuctionRules.Algorithm), "algorithm", auctionrunner.DefaultStartAuctionRules.Algorithm, "the auction algorithm to use")
	flag.IntVar(&(auctionrunner.DefaultStartAuctionRules.MaxRounds), "maxRounds", auctionrunner.DefaultStartAuctionRules.MaxRounds, "the maximum number of rounds per auction")
	flag.Float64Var(&(auctionrunner.DefaultStartAuctionRules.MaxBiddingPoolFraction), "maxBiddingPoolFraction", auctionrunner.DefaultStartAuctionRules.MaxBiddingPoolFraction, "the maximum number of participants in the pool")

	flag.IntVar(&maxConcurrentPerExecutor, "maxConcurrentPerExecutor", 20, "the maximum number of concurrent auctions to run, per executor")

	flag.BoolVar(&disableSVGReport, "disableSVGReport", false, "disable displaying SVG reports of the simulation runs")
	flag.StringVar(&reportName, "reportName", "report", "report name")
}

func TestAuction(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Auction Suite")
}

var _ = BeforeSuite(func() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Printf("Running in %s communicationMode\n", communicationMode)
	fmt.Printf("Running in %s auctioneerMode\n", auctioneerMode)

	startReport()

	logger := lager.NewLogger("auction-sim")
	logger.RegisterSink(lager.NewWriterSink(GinkgoWriter, lager.DEBUG))

	if auctioneerMode != InProcess && auctioneerMode != ExternalAuctioneerMode {
		panic(fmt.Sprintf("don't know about auctioneerMode: %s", auctioneerMode))
	}

	sessionsToTerminate = []*gexec.Session{}
	var mode string
	hosts := []string{}
	switch communicationMode {
	case InProcess:
		client, repAddresses = buildInProcessReps()
		if auctioneerMode == ExternalAuctioneerMode {
			panic("it doesn't make sense to use external auctioneers when the reps are in-process")
		}
	case NATS:
		natsAddrs := startNATS()
		var err error

		client, err = auction_nats_client.New(natsRunner.MessageBus, timeout, logger)
		Ω(err).ShouldNot(HaveOccurred())
		repAddresses = launchExternalNATSReps(natsAddrs)
		if auctioneerMode == ExternalAuctioneerMode {
			hosts = launchExternalNATSAuctioneers(natsAddrs)
		}
		mode = "NATS"
	case HTTP:
		repAddresses = launchExternalHTTPReps()

		client = auction_http_client.New(http.DefaultClient, logger)
		if auctioneerMode == ExternalAuctioneerMode {
			hosts = launchExternalHTTPAuctioneers()
		}
		mode = "HTTP"
	case DiegoNATSCommunicationMode, DiegoHTTPCommunicationMode:
		repAddresses = []auctiontypes.RepAddress{}
		for i := 1; i <= numReps; i++ {
			repAddresses = append(repAddresses, auctiontypes.RepAddress{
				RepGuid: fmt.Sprintf("rep-lite-%d", i),
				Address: fmt.Sprintf("http://rep-lite-%d.diego-1.cf-app.com", i),
			})
		}
		for i := 1; i <= numAuctioneers; i++ {
			hosts = append(hosts, fmt.Sprintf("auctioneer-lite-%d.diego-1.cf-app.com", i))
		}
		client = auction_http_client.New(http.DefaultClient, logger)
		auctioneerMode = ExternalAuctioneerMode
		if communicationMode == DiegoNATSCommunicationMode {
			mode = "NATS"
		} else {
			mode = "HTTP"
		}
	default:
		panic(fmt.Sprintf("unknown communication mode: %s", communicationMode))
	}

	if auctioneerMode == InProcess {
		auctionDistributor = auctiondistributor.NewInProcessAuctionDistributor(client, maxConcurrentPerExecutor)
	} else if auctioneerMode == ExternalAuctioneerMode {
		auctionDistributor = auctiondistributor.NewExternalAuctionDistributor(hosts, mode)
	}
})

var _ = BeforeEach(func() {
	wg := &sync.WaitGroup{}
	wg.Add(len(repAddresses))
	for _, repAddress := range repAddresses {
		repAddress := repAddress
		go func() {
			client.Reset(repAddress)
			wg.Done()
		}()
	}

	wg.Wait()

	util.ResetGuids()
})

var _ = AfterSuite(func() {
	if !disableSVGReport {
		finishReport()
	}

	for _, sess := range sessionsToTerminate {
		sess.Kill().Wait()
	}

	if natsRunner != nil {
		natsRunner.Stop()
	}
})

func buildInProcessReps() (auctiontypes.SimulationRepPoolClient, []auctiontypes.RepAddress) {
	inprocess.LatencyMin = LatencyMin
	inprocess.LatencyMax = LatencyMax

	repAddresses := []auctiontypes.RepAddress{}
	repMap := map[string]*auctionrep.AuctionRep{}

	for i := 0; i < numReps; i++ {
		repGuid := util.NewGuid("REP")
		repAddresses = append(repAddresses, auctiontypes.RepAddress{
			RepGuid: repGuid,
		})

		repDelegate := simulationrepdelegate.New(repResources)
		repMap[repGuid] = auctionrep.New(repGuid, repDelegate)
	}

	client := inprocess.New(repMap)
	return client, repAddresses
}

func startNATS() string {
	natsPort := 5222 + GinkgoParallelNode()
	natsAddrs := []string{fmt.Sprintf("127.0.0.1:%d", natsPort)}
	natsRunner = natsrunner.NewNATSRunner(natsPort)
	natsRunner.Start()
	return strings.Join(natsAddrs, ",")
}

func launchExternalNATSReps(natsAddrs string) []auctiontypes.RepAddress {
	repNodeBinary, err := gexec.Build("github.com/cloudfoundry-incubator/auction/simulation/local/repnode")
	Ω(err).ShouldNot(HaveOccurred())

	repAddresses := []auctiontypes.RepAddress{}

	for i := 0; i < numReps; i++ {
		repGuid := util.NewGuid("REP")

		serverCmd := exec.Command(
			repNodeBinary,
			"-repGuid", repGuid,
			"-natsAddrs", natsAddrs,
			"-memoryMB", fmt.Sprintf("%d", repResources.MemoryMB),
			"-diskMB", fmt.Sprintf("%d", repResources.DiskMB),
			"-containers", fmt.Sprintf("%d", repResources.Containers),
		)

		sess, err := gexec.Start(serverCmd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(sess).Should(gbytes.Say("listening"))
		sessionsToTerminate = append(sessionsToTerminate, sess)

		repAddresses = append(repAddresses, auctiontypes.RepAddress{
			RepGuid: repGuid,
		})
	}

	return repAddresses
}

func launchExternalHTTPReps() []auctiontypes.RepAddress {
	repNodeBinary, err := gexec.Build("github.com/cloudfoundry-incubator/auction/simulation/local/repnode")
	Ω(err).ShouldNot(HaveOccurred())

	repAddresses := []auctiontypes.RepAddress{}

	for i := 0; i < numReps; i++ {
		repGuid := util.NewGuid("REP")
		httpAddr := fmt.Sprintf("127.0.0.1:%d", 30000+i)

		serverCmd := exec.Command(
			repNodeBinary,
			"-repGuid", repGuid,
			"-httpAddr", httpAddr,
			"-memoryMB", fmt.Sprintf("%d", repResources.MemoryMB),
			"-diskMB", fmt.Sprintf("%d", repResources.DiskMB),
			"-containers", fmt.Sprintf("%d", repResources.Containers),
		)

		sess, err := gexec.Start(serverCmd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		sessionsToTerminate = append(sessionsToTerminate, sess)
		Eventually(sess).Should(gbytes.Say("listening"))

		repAddresses = append(repAddresses, auctiontypes.RepAddress{
			RepGuid: repGuid,
			Address: "http://" + httpAddr,
		})
	}

	return repAddresses
}

func launchExternalNATSAuctioneers(natsAddrs string) []string {
	auctioneerNodeBinary, err := gexec.Build("github.com/cloudfoundry-incubator/auction/simulation/local/auctioneernode")
	Ω(err).ShouldNot(HaveOccurred())

	auctioneerHosts := []string{}
	for i := 0; i < numAuctioneers; i++ {
		port := 48710 + i
		auctioneerCmd := exec.Command(
			auctioneerNodeBinary,
			"-natsAddrs", natsAddrs,
			"-timeout", fmt.Sprintf("%s", timeout),
			"-httpAddr", fmt.Sprintf("127.0.0.1:%d", port),
			"-maxConcurrent", fmt.Sprintf("%d", maxConcurrentPerExecutor),
		)
		auctioneerHosts = append(auctioneerHosts, fmt.Sprintf("127.0.0.1:%d", port))

		sess, err := gexec.Start(auctioneerCmd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(sess).Should(gbytes.Say("auctioneering"))
		sessionsToTerminate = append(sessionsToTerminate, sess)
	}

	return auctioneerHosts
}

func launchExternalHTTPAuctioneers() []string {
	auctioneerNodeBinary, err := gexec.Build("github.com/cloudfoundry-incubator/auction/simulation/local/auctioneernode")
	Ω(err).ShouldNot(HaveOccurred())

	auctioneerHosts := []string{}
	for i := 0; i < numAuctioneers; i++ {
		port := 48710 + i
		auctioneerCmd := exec.Command(
			auctioneerNodeBinary,
			"-timeout", fmt.Sprintf("%s", timeout),
			"-httpAddr", fmt.Sprintf("127.0.0.1:%d", port),
			"-maxConcurrent", fmt.Sprintf("%d", maxConcurrentPerExecutor),
		)
		auctioneerHosts = append(auctioneerHosts, fmt.Sprintf("127.0.0.1:%d", port))

		sess, err := gexec.Start(auctioneerCmd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(sess).Should(gbytes.Say("auctioneering"))
		sessionsToTerminate = append(sessionsToTerminate, sess)
	}

	return auctioneerHosts
}

func startReport() {
	svgReport = visualization.StartSVGReport("./"+reportName+".svg", 4, 4)
	svgReport.DrawHeader(communicationMode, auctionrunner.DefaultStartAuctionRules, maxConcurrentPerExecutor)
}

func finishReport() {
	svgReport.Done()
	_, err := exec.LookPath("rsvg-convert")
	if err == nil {
		exec.Command("rsvg-convert", "-h", "2000", "--background-color=#fff", "./"+reportName+".svg", "-o", "./"+reportName+".png").Run()
		exec.Command("open", "./"+reportName+".png").Run()
	}

	data, err := json.Marshal(reports)
	Ω(err).ShouldNot(HaveOccurred())
	ioutil.WriteFile("./"+reportName+".json", data, 0777)
}
