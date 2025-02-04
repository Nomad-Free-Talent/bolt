package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	builderApi "github.com/attestantio/go-builder-client/api"
	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	builderSpec "github.com/attestantio/go-builder-client/spec"
	eth2ApiV1Bellatrix "github.com/attestantio/go-eth2-client/api/v1/bellatrix"
	eth2ApiV1Capella "github.com/attestantio/go-eth2-client/api/v1/capella"
	eth2ApiV1Deneb "github.com/attestantio/go-eth2-client/api/v1/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	gethCommon "github.com/ethereum/go-ethereum/common"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	fastSsz "github.com/ferranbt/fastssz"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/go-boost-utils/utils"
	"github.com/flashbots/go-utils/httplogger"
	"github.com/flashbots/mev-boost/config"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

// Standard errors
var (
	errNoRelays                  = errors.New("no relays")
	errInvalidSlot               = errors.New("invalid slot")
	errInvalidHash               = errors.New("invalid hash")
	errInvalidPubkey             = errors.New("invalid pubkey")
	errNoSuccessfulRelayResponse = errors.New("no successful relay response")
	errServerAlreadyRunning      = errors.New("server already running")
)

// Bolt errors
var (
	errNilProof                  = errors.New("nil proof")
	errMissingConstraint         = errors.New("missing constraint")
	errMismatchProofSize         = errors.New("proof size mismatch")
	errInvalidProofs             = errors.New("proof verification failed")
	errInvalidRoot               = errors.New("failed getting tx root from bid")
	errNilConstraint             = errors.New("nil constraint")
	errHashesIndexesMismatch     = errors.New("proof transaction hashes and indexes length mismatch")
	errHashesConstraintsMismatch = errors.New("proof transaction hashes and constraints length mismatch")
)

var (
	nilHash     = phase0.Hash32{}
	nilResponse = struct{}{}
)

type httpErrorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// AuctionTranscript is the bid and blinded block received from the relay send to the relay monitor
type AuctionTranscript struct {
	Bid        *builderSpec.VersionedSignedBuilderBid       // TODO: proper json marshalling and unmarshalling
	Acceptance *eth2ApiV1Bellatrix.SignedBlindedBeaconBlock `json:"acceptance"`
}

type slotUID struct {
	slot uint64
	uid  uuid.UUID
}

// BoostServiceOpts provides all available options for use with NewBoostService
type BoostServiceOpts struct {
	Log                   *logrus.Entry
	ListenAddr            string
	Relays                []RelayEntry
	RelayMonitors         []*url.URL
	GenesisForkVersionHex string
	GenesisTime           uint64
	RelayCheck            bool
	RelayMinBid           types.U256Str

	RequestTimeoutGetHeader        time.Duration
	RequestTimeoutGetPayload       time.Duration
	RequestTimeoutRegVal           time.Duration
	RequestTimeoutSubmitConstraint time.Duration
	RequestMaxRetries              int
}

// BoostService - the mev-boost service
type BoostService struct {
	listenAddr    string
	relays        []RelayEntry
	relayMonitors []*url.URL
	log           *logrus.Entry
	srv           *http.Server
	relayCheck    bool
	relayMinBid   types.U256Str
	genesisTime   uint64

	builderSigningDomain       phase0.Domain
	httpClientGetHeader        http.Client
	httpClientGetPayload       http.Client
	httpClientRegVal           http.Client
	httpClientSubmitConstraint http.Client
	requestMaxRetries          int

	bids     map[bidRespKey]bidResp // keeping track of bids, to log the originating relay on withholding
	bidsLock sync.Mutex

	slotUID     *slotUID
	slotUIDLock sync.Mutex

	// BOLT: constraint cache
	constraints *ConstraintCache
}

// NewBoostService created a new BoostService
func NewBoostService(opts BoostServiceOpts) (*BoostService, error) {
	if len(opts.Relays) == 0 {
		return nil, errNoRelays
	}

	builderSigningDomain, err := ComputeDomain(ssz.DomainTypeAppBuilder, opts.GenesisForkVersionHex, phase0.Root{}.String())
	if err != nil {
		return nil, err
	}

	return &BoostService{
		listenAddr:    opts.ListenAddr,
		relays:        opts.Relays,
		relayMonitors: opts.RelayMonitors,
		log:           opts.Log,
		relayCheck:    opts.RelayCheck,
		relayMinBid:   opts.RelayMinBid,
		genesisTime:   opts.GenesisTime,
		bids:          make(map[bidRespKey]bidResp),
		slotUID:       &slotUID{},

		builderSigningDomain: builderSigningDomain,
		httpClientGetHeader: http.Client{
			Timeout:       opts.RequestTimeoutGetHeader,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientGetPayload: http.Client{
			Timeout:       opts.RequestTimeoutGetPayload,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientRegVal: http.Client{
			Timeout:       opts.RequestTimeoutRegVal,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientSubmitConstraint: http.Client{
			Timeout:       opts.RequestTimeoutSubmitConstraint,
			CheckRedirect: httpClientDisallowRedirects,
		},
		requestMaxRetries: opts.RequestMaxRetries,

		// BOLT: Initialize the constraint cache
		constraints: NewConstraintCache(64),
	}, nil
}

func (m *BoostService) respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := httpErrorResp{code, message}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		m.log.WithField("response", resp).WithError(err).Error("Couldn't write error response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) respondOK(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.log.WithField("response", response).WithError(err).Error("Couldn't write OK response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) getRouter() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/", m.handleRoot)

	r.HandleFunc(pathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(pathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)
	r.HandleFunc(pathSubmitConstraint, m.handleSubmitConstraint).Methods(http.MethodPost)
	// TODO: manage the switch between the endpoint with and without proofs
	// with the bolt sidecar proxy instead of using the same response here.
	// TODO: revert this to m.handleGetHeader
	r.HandleFunc(pathGetHeader, m.handleGetHeaderWithProofs).Methods(http.MethodGet)
	r.HandleFunc(pathGetHeaderWithProofs, m.handleGetHeaderWithProofs).Methods(http.MethodGet)
	r.HandleFunc(pathGetPayload, m.handleGetPayload).Methods(http.MethodPost)

	r.Use(mux.CORSMethodMiddleware(r))
	loggedRouter := httplogger.LoggingMiddlewareLogrus(m.log, r)
	return loggedRouter
}

// StartHTTPServer starts the HTTP server for this boost service instance
func (m *BoostService) StartHTTPServer() error {
	if m.srv != nil {
		return errServerAlreadyRunning
	}

	go m.startBidCacheCleanupTask()

	m.srv = &http.Server{
		Addr:    m.listenAddr,
		Handler: m.getRouter(),

		ReadTimeout:       time.Duration(config.ServerReadTimeoutMs) * time.Millisecond,
		ReadHeaderTimeout: time.Duration(config.ServerReadHeaderTimeoutMs) * time.Millisecond,
		WriteTimeout:      time.Duration(config.ServerWriteTimeoutMs) * time.Millisecond,
		IdleTimeout:       time.Duration(config.ServerIdleTimeoutMs) * time.Millisecond,

		MaxHeaderBytes: config.ServerMaxHeaderBytes,
	}

	err := m.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (m *BoostService) startBidCacheCleanupTask() {
	for {
		time.Sleep(1 * time.Minute)
		m.bidsLock.Lock()
		for k, bidResp := range m.bids {
			if time.Since(bidResp.t) > 3*time.Minute {
				delete(m.bids, k)
			}
		}
		m.bidsLock.Unlock()
	}
}

func (m *BoostService) sendValidatorRegistrationsToRelayMonitors(payload []builderApiV1.SignedValidatorRegistration) {
	log := m.log.WithField("method", "sendValidatorRegistrationsToRelayMonitors").WithField("numRegistrations", len(payload))
	for _, relayMonitor := range m.relayMonitors {
		go func(relayMonitor *url.URL) {
			url := GetURI(relayMonitor, pathRegisterValidator)
			log = log.WithField("url", url)
			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, url, "", nil, payload, nil)
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay monitor")
				return
			}
			log.Debug("sent validator registrations to relay monitor")
		}(relayMonitor)
	}
}

// func (m *BoostService) sendAuctionTranscriptToRelayMonitors(transcript *AuctionTranscript) {
// 	log := m.log.WithField("method", "sendAuctionTranscriptToRelayMonitors")
// 	for _, relayMonitor := range m.relayMonitors {
// 		go func(relayMonitor *url.URL) {
// 			url := GetURI(relayMonitor, pathAuctionTranscript)
// 			log := log.WithField("url", url)
// 			_, err := SendHTTPRequest(context.Background(), *http.DefaultClient, http.MethodPost, url, UserAgent(""), nil, transcript, nil)
// 			if err != nil {
// 				log.WithError(err).Warn("error sending auction transcript to relay monitor")
// 				return
// 			}
// 			log.Debug("sent auction transcript to relay monitor")
// 		}(relayMonitor)
// 	}
// }

func (m *BoostService) handleRoot(w http.ResponseWriter, _ *http.Request) {
	m.respondOK(w, nilResponse)
}

// handleStatus sends calls to the status endpoint of every relay.
// It returns OK if at least one returned OK, and returns error otherwise.
func (m *BoostService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(HeaderKeyVersion, config.Version)
	if !m.relayCheck || m.CheckRelays() > 0 {
		m.respondOK(w, nilResponse)
	} else {
		m.respondError(w, http.StatusServiceUnavailable, "all relays are unavailable")
	}
}

// handleRegisterValidator - returns 200 if at least one relay returns 200, else 502
func (m *BoostService) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "registerValidator")
	log.Debug("registerValidator")

	payload := []builderApiV1.SignedValidatorRegistration{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"numRegistrations": len(payload),
		"ua":               ua,
	})

	relayRespCh := make(chan error, len(m.relays))

	for _, relay := range m.relays {
		go func(relay RelayEntry) {
			url := relay.GetURI(pathRegisterValidator)
			log := log.WithField("url", url)

			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, url, ua, nil, payload, nil)
			relayRespCh <- err
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay")
				return
			}
		}(relay)
	}

	go m.sendValidatorRegistrationsToRelayMonitors(payload)

	for i := 0; i < len(m.relays); i++ {
		respErr := <-relayRespCh
		if respErr == nil {
			m.respondOK(w, nilResponse)
			return
		}
	}

	m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
}

// verifyInclusionProof verifies the proofs against the constraints, and returns an error if the proofs are invalid.
func (m *BoostService) verifyInclusionProof(transactionsRoot phase0.Root, proof *InclusionProof, slot uint64) error {
	log := m.log.WithFields(logrus.Fields{})

	// BOLT: get constraints for the slot
	inclusionConstraints, exists := m.constraints.Get(slot)

	if !exists {
		log.Warnf("[BOLT]: No constraints found for slot %d", slot)
		return errMissingConstraint
	}

	if proof == nil {
		return errNilProof
	}

	if len(proof.TransactionHashes) != len(inclusionConstraints) {
		return errMismatchProofSize
	}
	if len(proof.TransactionHashes) != len(proof.GeneralizedIndexes) {
		return errHashesIndexesMismatch
	}

	log.Infof("[BOLT]: Verifying merkle multiproofs for %d transactions", len(proof.TransactionHashes))

	// Decode the constraints, and sort them according to the utility function used
	// TODO: this should be done before verification ideally
	hashToConstraint := make(HashToConstraintDecoded)
	for hash, constraint := range inclusionConstraints {
		transaction := new(gethTypes.Transaction)
		err := transaction.UnmarshalBinary(constraint.Tx)
		if err != nil {
			log.WithError(err).Error("error unmarshalling transaction while verifying proofs")
			return err
		}
		hashToConstraint[hash] = &ConstraintDecoded{
			Tx:    transaction.WithoutBlobTxSidecar(),
			Index: constraint.Index,
		}
	}
	leaves := make([][]byte, len(inclusionConstraints))
	indexes := make([]int, len(proof.GeneralizedIndexes))

	for i, hash := range proof.TransactionHashes {
		constraint, ok := hashToConstraint[gethCommon.Hash(hash)]
		if constraint == nil || !ok {
			return errNilConstraint
		}

		// Compute the hash tree root for the raw preconfirmed transaction
		// and use it as "Leaf" in the proof to be verified against
		encoded, err := constraint.Tx.MarshalBinary()
		if err != nil {
			log.WithError(err).Error("error marshalling transaction without blob tx sidecar")
			return err
		}

		tx := Transaction(encoded)
		txHashTreeRoot, err := tx.HashTreeRoot()
		if err != nil {
			return errInvalidRoot
		}

		leaves[i] = txHashTreeRoot[:]
		indexes[i] = int(proof.GeneralizedIndexes[i])
		i++
	}

	hashes := make([][]byte, len(proof.MerkleHashes))
	for i, hash := range proof.MerkleHashes {
		hashes[i] = []byte(*hash)
	}

	currentTime := time.Now()
	ok, err := fastSsz.VerifyMultiproof(transactionsRoot[:], hashes, leaves, indexes)
	elapsed := time.Since(currentTime)
	if err != nil {
		log.WithError(err).Error("error verifying merkle proof")
		return err
	}

	if !ok {
		log.Error("[BOLT]: proof verification failed")

		// BOLT: send event to web demo
		message := fmt.Sprintf("failed to verify merkle proof for slot %d", slot)
		EmitBoltDemoEvent(message)

		return errInvalidProofs
	} else {
		log.Info(fmt.Sprintf("[BOLT]: merkle proof verified in %s", elapsed))

		// BOLT: send event to web demo
		// verified merkle proof for tx: %s in %v", proof.TxHash.String(), elapsed)
		message := fmt.Sprintf("verified merkle proof for slot %d in %v", slot, elapsed)
		EmitBoltDemoEvent(message)
	}

	return nil
}

// handleSubmitConstraint forwards a constraint to the relays, and registers them in the local cache.
// They will later be used to verify the proofs sent by the relays.
func (m *BoostService) handleSubmitConstraint(w http.ResponseWriter, req *http.Request) {
	ua := UserAgent(req.Header.Get("User-Agent"))
	log := m.log.WithFields(logrus.Fields{
		"method": "submitConstraint",
		"ua":     ua,
	})

	path := req.URL.Path

	log.Info("submitConstraint")

	payload := BatchedSignedConstraints{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		log.Error("error decoding payload: ", err)
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Add all constraints to the cache
	for _, signedConstraints := range payload {
		constraintMessage := signedConstraints.Message

		log.Infof("[BOLT]: adding inclusion constraints to cache. slot = %d, validatorIndex = %d, number of relays = %d", constraintMessage.Slot, constraintMessage.ValidatorIndex, len(m.relays))

		// Add the constraints to the cache.
		// They will be cleared when we receive a payload for the slot in `handleGetPayload`
		err := m.constraints.AddInclusionConstraints(constraintMessage.Slot, constraintMessage.Constraints)
		if err != nil {
			log.WithError(err).Errorf("error adding inclusion constraints to cache")
			continue
		}

		log.Infof("[BOLT]: added inclusion constraints to cache. slot = %d, validatorIndex = %d, number of relays = %d", constraintMessage.Slot, constraintMessage.ValidatorIndex, len(m.relays))
	}

	relayRespCh := make(chan error, len(m.relays))

	EmitBoltDemoEvent(fmt.Sprintf("received %d constraints, forwarding to Bolt relays... (path: %s)", len(payload), path))

	for _, relay := range m.relays {
		go func(relay RelayEntry) {
			url := relay.GetURI(pathSubmitConstraint)
			log := log.WithField("url", url)

			log.Infof("sending request for %d constraint to relay", len(payload))
			_, err := SendHTTPRequest(context.Background(), m.httpClientSubmitConstraint, http.MethodPost, url, ua, nil, payload, nil)
			log.Infof("sent request for %d constraint to relay. err = %v", len(payload), err)
			relayRespCh <- err
			if err != nil {
				log.WithError(err).Warn("error calling submitConstraint on relay")
				return
			}
		}(relay)
	}

	for i := 0; i < len(m.relays); i++ {
		respErr := <-relayRespCh
		if respErr == nil {
			m.respondOK(w, nilResponse)
			return
		}
	}

	m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
}

// handleGetHeader requests bids from the relays
func (m *BoostService) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	slot := vars["slot"]
	parentHashHex := vars["parent_hash"]
	pubkey := vars["pubkey"]

	ua := UserAgent(req.Header.Get("User-Agent"))
	log := m.log.WithFields(logrus.Fields{
		"method":     "getHeader",
		"slot":       slot,
		"parentHash": parentHashHex,
		"pubkey":     pubkey,
		"ua":         ua,
	})
	log.Debug("getHeader")

	_slot, err := strconv.ParseUint(slot, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	if len(pubkey) != 98 {
		m.respondError(w, http.StatusBadRequest, errInvalidPubkey.Error())
		return
	}

	if len(parentHashHex) != 66 {
		m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
		return
	}

	// Make sure we have a uid for this slot
	m.slotUIDLock.Lock()
	if m.slotUID.slot < _slot {
		m.slotUID.slot = _slot
		m.slotUID.uid = uuid.New()
	}
	slotUID := m.slotUID.uid
	m.slotUIDLock.Unlock()
	log = log.WithField("slotUID", slotUID)

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + _slot*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("getHeader request start - %d milliseconds into slot %d", msIntoSlot, _slot)

	// Add request headers
	headers := map[string]string{
		HeaderKeySlotUID: slotUID.String(),
	}

	// Prepare relay responses
	result := bidResp{}                           // the final response, containing the highest bid (if any)
	relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash

	// Call the relays
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/header/%s/%s/%s", slot, parentHashHex, pubkey)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(builderSpec.VersionedSignedBuilderBid)
			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, ua, headers, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if payload is empty
			if responsePayload.IsEmpty() {
				return
			}

			// Getting the bid info will check if there are missing fields in the response
			bidInfo, err := parseBidInfo(responsePayload)
			if err != nil {
				log.WithError(err).Warn("error parsing bid info")
				return
			}

			if bidInfo.blockHash == nilHash {
				log.Warn("relay responded with empty block hash")
				return
			}

			valueEth := weiBigIntToEthBigFloat(bidInfo.value.ToBig())
			log = log.WithFields(logrus.Fields{
				"blockNumber": bidInfo.blockNumber,
				"blockHash":   bidInfo.blockHash.String(),
				"txRoot":      bidInfo.txRoot.String(),
				"value":       valueEth.Text('f', 18),
			})

			if relay.PublicKey.String() != bidInfo.pubkey.String() {
				log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
				return
			}

			// Verify the relay signature in the relay response
			if !config.SkipRelaySignatureCheck {
				ok, err := checkRelaySignature(responsePayload, m.builderSigningDomain, relay.PublicKey)
				if err != nil {
					log.WithError(err).Error("error verifying relay signature")
					return
				}
				if !ok {
					log.Error("failed to verify relay signature")
					return
				}
			}

			// Verify response coherence with proposer's input data
			if bidInfo.parentHash.String() != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": bidInfo.parentHash.String(),
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			isZeroValue := bidInfo.value.IsZero()
			isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			if isZeroValue || isEmptyListTxRoot {
				log.Warn("ignoring bid with 0 value")
				return
			}
			log.Debug("bid received")

			// Skip if value (fee) is lower than the minimum bid
			if bidInfo.value.CmpBig(m.relayMinBid.BigInt()) == -1 {
				log.Debug("ignoring bid below min-bid value")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

			// Compare the bid with already known top bid (if any)
			if !result.response.IsEmpty() {
				valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
				if valueDiff == -1 { // current bid is less profitable than already known one
					return
				} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
					previousBidBlockHash := result.bidInfo.blockHash
					if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
						return
					}
				}
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Debug("new best bid")
			result.response = *responsePayload
			result.bidInfo = bidInfo
			result.t = time.Now()
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if result.response.IsEmpty() {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Log result
	valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	log.WithFields(logrus.Fields{
		"blockHash":   result.bidInfo.blockHash.String(),
		"blockNumber": result.bidInfo.blockNumber,
		"txRoot":      result.bidInfo.txRoot.String(),
		"value":       valueEth.Text('f', 18),
		"relays":      strings.Join(RelayEntriesToStrings(result.relays), ", "),
	}).Info("best bid")

	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: _slot, blockHash: result.bidInfo.blockHash.String()}
	m.bidsLock.Lock()
	m.bids[bidKey] = result
	m.bidsLock.Unlock()

	// Return the bid
	m.respondOK(w, &result.response)
}

// handleGetHeader requests bids from the relays
// BOLT: receiving preconfirmation proofs from relays along with bids, and
// verify them. If not valid, the bid is discarded
func (m *BoostService) handleGetHeaderWithProofs(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	slot := vars["slot"]
	parentHashHex := vars["parent_hash"]
	pubkey := vars["pubkey"]

	ua := UserAgent(req.Header.Get("User-Agent"))
	log := m.log.WithFields(logrus.Fields{
		"method":     "getHeaderWithProofs",
		"slot":       slot,
		"parentHash": parentHashHex,
		"pubkey":     pubkey,
		"ua":         ua,
	})
	log.Debug("getHeader")

	slotUint, err := strconv.ParseUint(slot, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	if len(pubkey) != 98 {
		m.respondError(w, http.StatusBadRequest, errInvalidPubkey.Error())
		return
	}

	if len(parentHashHex) != 66 {
		m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
		return
	}

	// Make sure we have a uid for this slot
	m.slotUIDLock.Lock()
	if m.slotUID.slot < slotUint {
		m.slotUID.slot = slotUint
		m.slotUID.uid = uuid.New()
	}
	slotUID := m.slotUID.uid
	m.slotUIDLock.Unlock()
	log = log.WithField("slotUID", slotUID)

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + slotUint*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("getHeader request start - %d milliseconds into slot %d", msIntoSlot, slotUint)

	// Add request headers
	headers := map[string]string{
		HeaderKeySlotUID: slotUID.String(),
	}

	// Prepare relay responses
	result := bidResp{}                           // the final response, containing the highest bid (if any)
	relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash

	// Call the relays
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/header_with_proofs/%s/%s/%s", slot, parentHashHex, pubkey)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(BidWithInclusionProofs)
			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, ua, headers, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if responsePayload.Proofs != nil {
				log.Infof("[BOLT]: get header with proofs at slot %s, received payload with proofs: %s", slot, responsePayload)
			}

			if code == http.StatusNoContent {
				log.Warn("no-content response")
				return
			}

			if responsePayload.Bid == nil {
				log.Warn("Bid in response is nil")
				return
			}

			// Skip if payload is empty
			if responsePayload.Bid.IsEmpty() {
				log.Warn("Bid is empty")
				return
			}

			// Getting the bid info will check if there are missing fields in the response
			bidInfo, err := parseBidInfo(responsePayload.Bid)
			if err != nil {
				log.WithError(err).Warn("error parsing bid info")
				return
			}

			if bidInfo.blockHash == nilHash {
				log.Warn("relay responded with empty block hash")
				return
			}

			valueEth := weiBigIntToEthBigFloat(bidInfo.value.ToBig())
			log = log.WithFields(logrus.Fields{
				"blockNumber": bidInfo.blockNumber,
				"blockHash":   bidInfo.blockHash.String(),
				"txRoot":      bidInfo.txRoot.String(),
				"value":       valueEth.Text('f', 18),
			})

			if relay.PublicKey.String() != bidInfo.pubkey.String() {
				log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
				return
			}

			// Verify the relay signature in the relay response
			if !config.SkipRelaySignatureCheck {
				ok, err := checkRelaySignature(responsePayload.Bid, m.builderSigningDomain, relay.PublicKey)
				if err != nil {
					log.WithError(err).Error("error verifying relay signature")
					return
				}
				if !ok {
					log.Error("failed to verify relay signature")
					return
				}
			}

			// Verify response coherence with proposer's input data
			if bidInfo.parentHash.String() != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": bidInfo.parentHash.String(),
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			isZeroValue := bidInfo.value.IsZero()
			isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			if isZeroValue || isEmptyListTxRoot {
				log.Warn("ignoring bid with 0 value")
				return
			}
			log.Debug("bid received")

			// Skip if value (fee) is lower than the minimum bid
			if bidInfo.value.CmpBig(m.relayMinBid.BigInt()) == -1 {
				log.Warn("ignoring bid below min-bid value")
				return
			}

			// BOLT: verify preconfirmation inclusion proofs. If they don't match, we don't consider the bid to be valid.
			if responsePayload.Proofs != nil {
				// BOLT: verify the proofs against the constraints. If they don't match, we don't consider the bid to be valid.
				transactionsRoot, err := responsePayload.Bid.TransactionsRoot()
				if err != nil {
					log.WithError(err).Error("[BOLT]: error getting transaction root")
					return
				}
				if err := m.verifyInclusionProof(transactionsRoot, responsePayload.Proofs, slotUint); err != nil {
					log.Warnf("[BOLT]: Proof verification failed for relay %s: %s", relay.URL, err)
					return
				}
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

			// Compare the bid with already known top bid (if any)
			if !result.response.IsEmpty() {
				valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
				if valueDiff == -1 { // current bid is less profitable than already known one
					return
				} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
					previousBidBlockHash := result.bidInfo.blockHash
					if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
						return
					}
				}
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Infof("new best bid. Has proofs: %v", responsePayload.Proofs != nil)
			result.response = *responsePayload.Bid
			result.bidInfo = bidInfo
			result.t = time.Now()
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if result.response.IsEmpty() {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Log result
	valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	log.WithFields(logrus.Fields{
		"blockHash":   result.bidInfo.blockHash.String(),
		"blockNumber": result.bidInfo.blockNumber,
		"txRoot":      result.bidInfo.txRoot.String(),
		"value":       valueEth.Text('f', 18),
		"relays":      strings.Join(RelayEntriesToStrings(result.relays), ", "),
	}).Infof("best bid")

	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: slotUint, blockHash: result.bidInfo.blockHash.String()}
	m.bidsLock.Lock()
	m.bids[bidKey] = result
	m.bidsLock.Unlock()

	// Return the bid
	m.respondOK(w, &result.response)
	log.Infof("responded with best bid to beacon client")
}

func (m *BoostService) processCapellaPayload(w http.ResponseWriter, req *http.Request, log *logrus.Entry, payload *eth2ApiV1Capella.SignedBlindedBeaconBlock, body []byte) {
	if payload.Message == nil || payload.Message.Body == nil || payload.Message.Body.ExecutionPayloadHeader == nil {
		log.WithField("body", string(body)).Error("missing parts of the request payload from the beacon-node")
		m.respondError(w, http.StatusBadRequest, "missing parts of the payload")
		return
	}

	// Get the slotUID for this slot
	slotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == uint64(payload.Message.Slot) {
		slotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, payload.Message.Slot)
	}
	m.slotUIDLock.Unlock()

	// Prepare logger
	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       payload.Message.Slot,
		"blockHash":  payload.Message.Body.ExecutionPayloadHeader.BlockHash.String(),
		"parentHash": payload.Message.Body.ExecutionPayloadHeader.ParentHash.String(),
		"slotUID":    slotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + uint64(payload.Message.Slot)*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, payload.Message.Slot)

	// Get the bid!
	bidKey := bidRespKey{slot: uint64(payload.Message.Slot), blockHash: payload.Message.Body.ExecutionPayloadHeader.BlockHash.String()}
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey]
	m.bidsLock.Unlock()
	if originalBid.response.IsEmpty() {
		log.Error("no bid for this getPayload payload found. was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// send bid and signed block to relay monitor with eth2ApiV1Capella payload
	// go m.sendAuctionTranscriptToRelayMonitors(&AuctionTranscript{Bid: originalBid.response.Data, Acceptance: payload})

	// Add request headers
	headers := map[string]string{HeaderKeySlotUID: slotUID}

	// Prepare for requests
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := new(builderApi.VersionedSubmitBlindedBlockResponse)

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(builderApi.VersionedSubmitBlindedBlockResponse)
			_, err := SendHTTPRequestWithRetries(requestCtx, m.httpClientGetPayload, http.MethodPost, url, ua, headers, payload, responsePayload, m.requestMaxRetries, log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if getPayloadResponseIsEmpty(responsePayload) {
				log.Error("response with empty data!")
				return
			}

			// Ensure the response blockhash matches the request
			if payload.Message.Body.ExecutionPayloadHeader.BlockHash != responsePayload.Capella.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": responsePayload.Capella.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			// Ensure the response blockhash matches the response block
			calculatedBlockHash, err := utils.ComputeBlockHash(&builderApi.VersionedExecutionPayload{
				Version: responsePayload.Version,
				Capella: responsePayload.Capella,
			}, nil)
			if err != nil {
				log.WithError(err).Error("could not calculate block hash")
			} else if responsePayload.Capella.BlockHash != calculatedBlockHash {
				log.WithFields(logrus.Fields{
					"calculatedBlockHash": calculatedBlockHash.String(),
					"responseBlockHash":   responsePayload.Capella.BlockHash.String(),
				}).Error("responseBlockHash does not equal hash calculated from response block")
			}

			// Lock before accessing the shared payload
			mu.Lock()
			defer mu.Unlock()

			if requestCtx.Err() != nil { // request has been cancelled (or deadline exceeded)
				return
			}

			// Received successful response. Now cancel other requests and return immediately
			requestCtxCancel()
			*result = *responsePayload
			log.Info("received payload from relay")
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	// If no payload has been received from relay, log loudly about withholding!
	if result.Capella == nil || result.Capella.BlockHash == nilHash {
		originRelays := RelayEntriesToStrings(originalBid.relays)
		log.WithField("relaysWithBid", strings.Join(originRelays, ", ")).Error("no payload received from relay!")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}

	m.respondOK(w, result)
}

func (m *BoostService) processDenebPayload(w http.ResponseWriter, req *http.Request, log *logrus.Entry, blindedBlock *eth2ApiV1Deneb.SignedBlindedBeaconBlock) {
	// Get the currentSlotUID for this slot
	currentSlotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == uint64(blindedBlock.Message.Slot) {
		currentSlotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, blindedBlock.Message.Slot)
	}
	m.slotUIDLock.Unlock()

	// Prepare logger
	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       blindedBlock.Message.Slot,
		"blockHash":  blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash.String(),
		"parentHash": blindedBlock.Message.Body.ExecutionPayloadHeader.ParentHash.String(),
		"slotUID":    currentSlotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + uint64(blindedBlock.Message.Slot)*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, blindedBlock.Message.Slot)

	// Get the bid!
	bidKey := bidRespKey{slot: uint64(blindedBlock.Message.Slot), blockHash: blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash.String()}
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey]
	m.bidsLock.Unlock()
	if originalBid.response.IsEmpty() {
		log.Error("no bid for this getPayload payload found, was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// Add request headers
	headers := map[string]string{HeaderKeySlotUID: currentSlotUID}

	// Prepare for requests
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := new(builderApi.VersionedSubmitBlindedBlockResponse)

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(builderApi.VersionedSubmitBlindedBlockResponse)
			_, err := SendHTTPRequestWithRetries(requestCtx, m.httpClientGetPayload, http.MethodPost, url, ua, headers, blindedBlock, responsePayload, m.requestMaxRetries, log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if getPayloadResponseIsEmpty(responsePayload) {
				log.Error("response with empty data!")
				return
			}

			payload := responsePayload.Deneb.ExecutionPayload
			blobs := responsePayload.Deneb.BlobsBundle

			// Ensure the response blockhash matches the request
			if blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash != payload.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": payload.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			commitments := blindedBlock.Message.Body.BlobKZGCommitments
			// Ensure that blobs are valid and matches the request
			if len(commitments) != len(blobs.Blobs) || len(commitments) != len(blobs.Commitments) || len(commitments) != len(blobs.Proofs) {
				log.WithFields(logrus.Fields{
					"requestBlobCommitments":  len(commitments),
					"responseBlobs":           len(blobs.Blobs),
					"responseBlobCommitments": len(blobs.Commitments),
					"responseBlobProofs":      len(blobs.Proofs),
				}).Error("block KZG commitment length does not equal responseBlobs length")
				return
			}

			for i, commitment := range commitments {
				if commitment != blobs.Commitments[i] {
					log.WithFields(logrus.Fields{
						"requestBlobCommitment":  commitment.String(),
						"responseBlobCommitment": blobs.Commitments[i].String(),
						"index":                  i,
					}).Error("requestBlobCommitment does not equal responseBlobCommitment")
					return
				}
			}

			// Lock before accessing the shared payload
			mu.Lock()
			defer mu.Unlock()

			if requestCtx.Err() != nil { // request has been cancelled (or deadline exceeded)
				return
			}

			// Received successful response. Now cancel other requests and return immediately
			requestCtxCancel()
			*result = *responsePayload
			log.Info("received payload from relay")
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	// If no payload has been received from relay, log loudly about withholding!
	if getPayloadResponseIsEmpty(result) {
		originRelays := RelayEntriesToStrings(originalBid.relays)
		log.WithField("relaysWithBid", strings.Join(originRelays, ", ")).Error("no payload received from relay!")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}

	m.respondOK(w, result)
}

// handleGetPayload submits a signed blinded header to receive the payload body from the relays.
// BOLT: when receiving the payload, we also remove the associated constraints for this slot.
func (m *BoostService) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	// Read the body first, so we can log it later on error
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).Error("could not read body of request from the beacon node")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Decode the body now
	payload := new(eth2ApiV1Deneb.SignedBlindedBeaconBlock)
	if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
		log.Debug("could not decode Deneb request payload, attempting to decode body into Capella payload")
		payload := new(eth2ApiV1Capella.SignedBlindedBeaconBlock)
		if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
			log.WithError(err).WithField("body", string(body)).Error("could not decode request payload from the beacon-node (signed blinded beacon block)")
			m.respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		m.processCapellaPayload(w, req, log, payload, body)
		return
	}

	m.processDenebPayload(w, req, log, payload)
}

// CheckRelays sends a request to each one of the relays previously registered to get their status
func (m *BoostService) CheckRelays() int {
	var wg sync.WaitGroup
	var numSuccessRequestsToRelay uint32

	for _, r := range m.relays {
		wg.Add(1)

		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathStatus)
			log := m.log.WithField("url", url)
			log.Debug("checking relay status")

			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, "", nil, nil, nil)
			if err != nil {
				log.WithError(err).Error("relay status error - request failed")
				return
			}
			if code == http.StatusOK {
				log.Debug("relay status OK")
			} else {
				log.Errorf("relay status error - unexpected status code %d", code)
				return
			}

			// Success: increase counter and cancel all pending requests to other relays
			atomic.AddUint32(&numSuccessRequestsToRelay, 1)
		}(r)
	}

	// At the end, wait for every routine and return status according to relay's ones.
	wg.Wait()
	return int(numSuccessRequestsToRelay)
}
