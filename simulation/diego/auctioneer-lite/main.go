package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cloudfoundry-incubator/auction/communication/nats/auction_nats_client"

	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/gunk/workpool"
	"github.com/cloudfoundry/yagnats"

	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"

	"github.com/cloudfoundry-incubator/runtime-schema/bbs"

	"github.com/cloudfoundry-incubator/auction/communication/http/auction_http_client"

	"github.com/cloudfoundry-incubator/auction/auctionrunner"
	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/cloudfoundry-incubator/cf-lager"
)

var timeout = flag.Duration("timeout", time.Second, "timeout for nats responses")
var etcdCluster = flag.String("etcdCluster", "", "etcd cluster")
var natsUsername = flag.String("natsUsername", "", "nats username")
var natsPassword = flag.String("natsPassword", "", "nats password")
var natsAddresses = flag.String("natsAddresses", "", "nats addresses")
var maxConcurrent = flag.Int("maxConcurrent", 20, "max concurrent auctions")

var lookupTable map[string]string
var lookupTableLock *sync.RWMutex

func FetchLookupTable() {
	store := etcdstoreadapter.NewETCDStoreAdapter(strings.Split(*etcdCluster, ","), workpool.NewWorkPool(10))
	store.Connect()
	BBS := bbs.NewBBS(store, timeprovider.NewTimeProvider(), cf_lager.New("auctioneer-bbs"))

	actuals, err := BBS.GetAllActualLRPs()
	if err != nil {
		log.Fatalln("failed to fetch reps from etcd", err)
	}

	lookupTableLock.Lock()
	lookupTable = map[string]string{}
	for _, actual := range actuals {
		if strings.HasPrefix(actual.ProcessGuid, "rep-lite") && len(actual.Ports) == 1 {
			lookupTable[actual.ProcessGuid] = fmt.Sprintf("http://%s:%d", actual.Host, actual.Ports[0].HostPort)
		}
	}
	lookupTableLock.Unlock()
}

func AddressLookup(repGuid string) (string, error) {
	lookupTableLock.RLock()
	defer lookupTableLock.RUnlock()

	if lookupTable == nil {
		return "", errors.New("lookupTable uninitialized")
	}

	address, ok := lookupTable[repGuid]
	if !ok {
		return "", errors.New("unkown rep-guid: " + repGuid)
	}

	return address, nil
}

func transformRepAddresses(repAddresses []auctiontypes.RepAddress) []auctiontypes.RepAddress {
	transformed := []auctiontypes.RepAddress{}
	for _, repAddress := range repAddresses {
		address, err := AddressLookup(repAddress.RepGuid)
		if err != nil {
			fmt.Println(err.Error())
			continue
		}
		transformed = append(transformed, auctiontypes.RepAddress{
			RepGuid: repAddress.RepGuid,
			Address: address,
		})
	}

	return transformed
}

func main() {
	flag.Parse()
	lookupTableLock = &sync.RWMutex{}

	if *etcdCluster == "" {
		log.Fatalln("you must provide an etcd cluster")
	}

	repNATSClient := connectToNATS()

	FetchLookupTable()

	var repHTTPClient auctiontypes.RepPoolClient
	repHTTPClient = auction_http_client.New(&http.Client{
		Timeout: *timeout,
	}, cf_lager.New("auctioneer-http"))

	workers := workpool.NewWorkPool(*maxConcurrent)

	getCommunicationMode := func(r *http.Request) (auctiontypes.RepPoolClient, bool) {
		var httpMode bool
		var repClient auctiontypes.RepPoolClient
		if r.URL.Query().Get("mode") == "NATS" {
			repClient = repNATSClient
			httpMode = false
		} else {
			repClient = repHTTPClient
			httpMode = true
		}
		return repClient, httpMode
	}

	http.HandleFunc("/start-auctions", func(w http.ResponseWriter, r *http.Request) {
		var auctionRequests []auctiontypes.StartAuctionRequest
		err := json.NewDecoder(r.Body).Decode(&auctionRequests)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		repClient, httpMode := getCommunicationMode(r)

		lock := &sync.Mutex{}
		wg := &sync.WaitGroup{}
		wg.Add(len(auctionRequests))
		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		for _, auctionRequest := range auctionRequests {
			auctionRequest := auctionRequest
			workers.Submit(func() {
				if httpMode {
					auctionRequest.RepAddresses = transformRepAddresses(auctionRequest.RepAddresses)
				}
				auctionResult, _ := auctionrunner.New(repClient).RunLRPStartAuction(auctionRequest)
				lock.Lock()
				encoder.Encode(auctionResult)
				lock.Unlock()
				wg.Done()
			})
		}

		wg.Wait()
	})

	http.HandleFunc("/stop-auctions", func(w http.ResponseWriter, r *http.Request) {
		var auctionRequests []auctiontypes.StopAuctionRequest
		err := json.NewDecoder(r.Body).Decode(&auctionRequests)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		repClient, httpMode := getCommunicationMode(r)

		lock := &sync.Mutex{}
		wg := &sync.WaitGroup{}
		wg.Add(len(auctionRequests))
		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		for _, auctionRequest := range auctionRequests {
			auctionRequest := auctionRequest
			workers.Submit(func() {
				if httpMode {
					auctionRequest.RepAddresses = transformRepAddresses(auctionRequest.RepAddresses)
				}
				auctionResult, _ := auctionrunner.New(repClient).RunLRPStopAuction(auctionRequest)
				lock.Lock()
				encoder.Encode(auctionResult)
				lock.Unlock()
				wg.Done()
			})
		}

		wg.Wait()
	})

	http.HandleFunc("/routes", func(w http.ResponseWriter, r *http.Request) {
		FetchLookupTable()
		lookupTableLock.RLock()
		defer lookupTableLock.RUnlock()
		json.NewEncoder(w).Encode(lookupTable)
	})

	fmt.Println("auctioneering")

	panic(http.ListenAndServe("0.0.0.0:8080", nil))
}

func connectToNATS() auctiontypes.RepPoolClient {
	if *natsAddresses != "" && *natsUsername != "" && *natsPassword != "" {
		natsMembers := []string{}
		for _, addr := range strings.Split(*natsAddresses, ",") {
			uri := url.URL{
				Scheme: "nats",
				Host:   addr,
				User:   url.UserPassword(*natsUsername, *natsPassword),
			}
			natsMembers = append(natsMembers, uri.String())
		}

		client, err := yagnats.Connect(natsMembers)
		if err != nil {
			log.Fatalln("no nats:", err)
		}

		repClient, err := auction_nats_client.New(client, *timeout, cf_lager.New("auctioneer-nats"))
		if err != nil {
			log.Fatalln("no rep client:", err)
		}

		return repClient
	}

	return nil
}
