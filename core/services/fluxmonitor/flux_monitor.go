package fluxmonitor

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"sync"
	"time"

	"chainlink/core/logger"
	"chainlink/core/services/eth"
	"chainlink/core/services/eth/contracts"
	"chainlink/core/store"
	"chainlink/core/store/models"
	"chainlink/core/store/orm"
	"chainlink/core/utils"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
)

//go:generate mockery -name Service -output ../../internal/mocks/ -case=underscore
//go:generate mockery -name DeviationCheckerFactory -output ../../internal/mocks/ -case=underscore
//go:generate mockery -name DeviationChecker -output ../../internal/mocks/ -case=underscore

// defaultHTTPTimeout is the timeout used by the price adapter fetcher for outgoing HTTP requests.
const defaultHTTPTimeout = 5 * time.Second

// MinimumPollingInterval is the smallest possible polling interval the Flux
// Monitor supports.
const MinimumPollingInterval = models.Duration(defaultHTTPTimeout)

type RunManager interface {
	Create(
		jobSpecID *models.ID,
		initiator *models.Initiator,
		creationHeight *big.Int,
		runRequest *models.RunRequest,
	) (*models.JobRun, error)
}

// Service is the interface encapsulating all functionality
// needed to listen to price deviations and new round requests.
type Service interface {
	AddJob(models.JobSpec) error
	RemoveJob(*models.ID)
	Start() error
	Stop()
}

type concreteFluxMonitor struct {
	store          *store.Store
	runManager     RunManager
	logBroadcaster eth.LogBroadcaster
	checkerFactory DeviationCheckerFactory
	chAdd          chan addEntry
	chRemove       chan models.ID
	chConnect      chan *models.Head
	chDisconnect   chan struct{}
	chStop         chan struct{}
	chDone         chan struct{}
}

type addEntry struct {
	jobID    models.ID
	checkers []DeviationChecker
}

// New creates a service that manages a collection of DeviationCheckers,
// one per initiator of type InitiatorFluxMonitor for added jobs.
func New(
	store *store.Store,
	runManager RunManager,
) Service {
	logBroadcaster := eth.NewLogBroadcaster(store.TxManager, store.ORM)
	return &concreteFluxMonitor{
		store:          store,
		runManager:     runManager,
		logBroadcaster: logBroadcaster,
		checkerFactory: pollingDeviationCheckerFactory{
			store:          store,
			logBroadcaster: logBroadcaster,
		},
		chAdd:        make(chan addEntry),
		chRemove:     make(chan models.ID),
		chConnect:    make(chan *models.Head),
		chDisconnect: make(chan struct{}),
		chStop:       make(chan struct{}),
		chDone:       make(chan struct{}),
	}
}

func (fm *concreteFluxMonitor) Start() error {
	fm.logBroadcaster.Start()

	go fm.processAddRemoveJobRequests()

	var wg sync.WaitGroup
	err := fm.store.Jobs(func(j *models.JobSpec) bool {
		if j == nil {
			err := errors.New("received nil job")
			logger.Error(err)
			return true
		}
		job := *j

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := fm.AddJob(job)
			if err != nil {
				logger.Errorf("error adding FluxMonitor job: %v", err)
			}
		}()
		return true
	}, models.InitiatorFluxMonitor)

	wg.Wait()

	return err
}

// Disconnect cleans up running deviation checkers.
func (fm *concreteFluxMonitor) Stop() {
	fm.logBroadcaster.Stop()
	close(fm.chStop)
	<-fm.chDone
}

// actionConsumer is the CSP consumer. It's run on a single goroutine to
// coordinate the collection of DeviationCheckers in a thread-safe fashion.
func (fm *concreteFluxMonitor) processAddRemoveJobRequests() {
	defer close(fm.chDone)

	jobMap := map[models.ID][]DeviationChecker{}

	for {
		select {
		case entry := <-fm.chAdd:
			if _, ok := jobMap[entry.jobID]; ok {
				logger.Errorf("job %s has already been added to flux monitor", entry.jobID)
				return
			}
			for _, checker := range entry.checkers {
				checker.Start()
			}
			jobMap[entry.jobID] = entry.checkers

		case jobID := <-fm.chRemove:
			for _, checker := range jobMap[jobID] {
				checker.Stop()
			}
			delete(jobMap, jobID)

		case <-fm.chStop:
			for _, checkers := range jobMap {
				for _, checker := range checkers {
					checker.Stop()
				}
			}
			return
		}
	}
}

// AddJob created a DeviationChecker for any job initiators of type
// InitiatorFluxMonitor.
func (fm *concreteFluxMonitor) AddJob(job models.JobSpec) error {
	if job.ID == nil {
		err := errors.New("received job with nil ID")
		logger.Error(err)
		return err
	}

	var validCheckers []DeviationChecker
	for _, initr := range job.InitiatorsFor(models.InitiatorFluxMonitor) {
		logger.Debugw("Adding job to flux monitor",
			"job", job.ID.String(),
			"initr", initr.ID,
		)
		checker, err := fm.checkerFactory.New(initr, fm.runManager, fm.store.ORM)
		if err != nil {
			return errors.Wrap(err, "factory unable to create checker")
		}
		validCheckers = append(validCheckers, checker)
	}
	if len(validCheckers) == 0 {
		return nil
	}

	fm.chAdd <- addEntry{*job.ID, validCheckers}
	return nil
}

// RemoveJob stops and removes the checker for all Flux Monitor initiators belonging
// to the passed job ID.
func (fm *concreteFluxMonitor) RemoveJob(id *models.ID) {
	if id == nil {
		logger.Warn("nil job ID passed to FluxMonitor#RemoveJob")
		return
	}
	fm.chRemove <- *id
}

// DeviationCheckerFactory holds the New method needed to create a new instance
// of a DeviationChecker.
type DeviationCheckerFactory interface {
	New(models.Initiator, RunManager, *orm.ORM) (DeviationChecker, error)
}

type pollingDeviationCheckerFactory struct {
	store          *store.Store
	logBroadcaster eth.LogBroadcaster
}

func (f pollingDeviationCheckerFactory) New(initr models.Initiator, runManager RunManager, orm *orm.ORM) (DeviationChecker, error) {
	if initr.InitiatorParams.PollingInterval < MinimumPollingInterval {
		return nil, fmt.Errorf("pollingInterval must be equal or greater than %s", MinimumPollingInterval)
	}

	urls, err := ExtractFeedURLs(initr.InitiatorParams.Feeds, orm)
	if err != nil {
		return nil, err
	}

	fetcher, err := newMedianFetcherFromURLs(
		defaultHTTPTimeout,
		initr.InitiatorParams.RequestData.String(),
		urls)
	if err != nil {
		return nil, err
	}

	fluxAggregator, err := contracts.NewFluxAggregator(initr.InitiatorParams.Address, f.store.TxManager, f.logBroadcaster)
	if err != nil {
		return nil, err
	}

	return NewPollingDeviationChecker(
		f.store,
		fluxAggregator,
		initr,
		runManager,
		fetcher,
		initr.InitiatorParams.PollingInterval.Duration(),
	)
}

// ExtractFeedURLs extracts a list of url.URLs from the feeds parameter of the initiator params
func ExtractFeedURLs(feeds models.Feeds, orm *orm.ORM) ([]*url.URL, error) {
	var feedsData []interface{}
	var urls []*url.URL

	err := json.Unmarshal(feeds.Bytes(), &feedsData)
	if err != nil {
		return nil, err
	}

	for _, entry := range feedsData {
		var bridgeURL *url.URL
		var err error

		switch feed := entry.(type) {
		case string: // feed url - ex: "http://example.com"
			bridgeURL, err = url.ParseRequestURI(feed)
		case map[string]interface{}: // named feed - ex: {"bridge": "bridgeName"}
			bridgeName := feed["bridge"].(string)
			bridgeURL, err = GetBridgeURLFromName(bridgeName, orm) // XXX: currently an n query
		default:
			err = errors.New("unable to extract feed URLs from json")
		}

		if err != nil {
			return nil, err
		}
		urls = append(urls, bridgeURL)
	}

	return urls, nil
}

// GetBridgeURLFromName looks up a bridge in the DB by name, then extracts the url
func GetBridgeURLFromName(name string, orm *orm.ORM) (*url.URL, error) {
	task := models.TaskType(name)
	bridge, err := orm.FindBridge(task)
	if err != nil {
		return nil, err
	}
	bridgeURL := url.URL(bridge.URL)
	return &bridgeURL, nil
}

// DeviationChecker encapsulate methods needed to initialize and check prices
// for price deviations.
type DeviationChecker interface {
	Start()
	Stop()
}

// PollingDeviationChecker polls external price adapters via HTTP to check for price swings.
type PollingDeviationChecker struct {
	store          *store.Store
	fluxAggregator contracts.FluxAggregator
	runManager     RunManager
	fetcher        Fetcher

	initr         models.Initiator
	requestData   models.JSON
	threshold     float64
	precision     int32
	idleThreshold time.Duration

	connected                  utils.AtomicBool
	chMaybeLogs                chan maybeLog
	reportableRoundID          *big.Int
	mostRecentSubmittedRoundID uint64
	pollTicker                 *ResettableTicker
	idleTicker                 <-chan time.Time

	chStop     chan struct{}
	waitOnStop chan struct{}
}

// maybeLog is just a tuple that allows us to send either an error or a log over the
// logs channel.  This is preferable to using two separate channels, as it ensures
// that we don't drop valid (but unprocessed) logs if we receive an error.
type maybeLog struct {
	Log interface{}
	Err error
}

// NewPollingDeviationChecker returns a new instance of PollingDeviationChecker.
func NewPollingDeviationChecker(
	store *store.Store,
	fluxAggregator contracts.FluxAggregator,
	initr models.Initiator,
	runManager RunManager,
	fetcher Fetcher,
	pollDelay time.Duration,
) (*PollingDeviationChecker, error) {
	return &PollingDeviationChecker{
		store:          store,
		fluxAggregator: fluxAggregator,
		initr:          initr,
		requestData:    initr.InitiatorParams.RequestData,
		idleThreshold:  initr.InitiatorParams.IdleThreshold.Duration(),
		threshold:      float64(initr.InitiatorParams.Threshold),
		precision:      initr.InitiatorParams.Precision,
		runManager:     runManager,
		fetcher:        fetcher,
		pollTicker:     NewResettableTicker(pollDelay),
		idleTicker:     nil,
		chMaybeLogs:    make(chan maybeLog, 100),
		chStop:         make(chan struct{}),
		waitOnStop:     make(chan struct{}),
	}, nil
}

// Start begins the CSP consumer in a single goroutine to
// poll the price adapters and listen to NewRound events.
func (p *PollingDeviationChecker) Start() {
	logger.Debugw("Starting checker for job",
		"job", p.initr.JobSpecID.String(),
		"initr", p.initr.ID)

	go p.consume()
}

// Stop stops this instance from polling, cleaning up resources.
func (p *PollingDeviationChecker) Stop() {
	close(p.chStop)
	<-p.waitOnStop
}

func (p *PollingDeviationChecker) OnConnect() {
	logger.Debugw("PollingDeviationChecker connected to Ethereum node",
		"address", p.initr.InitiatorParams.Address.Hex(),
	)
	p.connected.Set(true)
}

func (p *PollingDeviationChecker) OnDisconnect() {
	logger.Debugw("PollingDeviationChecker disconnected from Ethereum node",
		"address", p.initr.InitiatorParams.Address.Hex(),
	)
	p.connected.Set(false)
}

type ResettableTicker struct {
	*time.Ticker
	d time.Duration
}

func NewResettableTicker(d time.Duration) *ResettableTicker {
	return &ResettableTicker{nil, d}
}

func (t *ResettableTicker) Tick() <-chan time.Time {
	if t.Ticker == nil {
		return nil
	}
	return t.Ticker.C
}

func (t *ResettableTicker) Stop() {
	if t.Ticker != nil {
		t.Ticker.Stop()
		t.Ticker = nil
	}
}

func (t *ResettableTicker) Reset() {
	t.Stop()
	t.Ticker = time.NewTicker(t.d)
}

func (p *PollingDeviationChecker) HandleLog(log interface{}, err error) {
	select {
	case p.chMaybeLogs <- maybeLog{log, err}:
	case <-p.chStop:
	}
}

func (p *PollingDeviationChecker) consume() {
	defer close(p.waitOnStop)

	p.determineMostRecentSubmittedRoundID()

	connected, unsubscribeLogs := p.fluxAggregator.SubscribeToLogs(p)
	defer unsubscribeLogs()

	p.connected.Set(connected)

	// Try to do an initial poll
	p.pollIfEligible(p.threshold)
	p.pollTicker.Reset()
	defer p.pollTicker.Stop()

	if p.idleThreshold > 0 {
		p.idleTicker = time.After(p.idleThreshold)
	}

Loop:
	for {
		select {
		case <-p.chStop:
			return

		case maybeLog := <-p.chMaybeLogs:
			if maybeLog.Err != nil {
				logger.Errorf("error received from log broadcaster: %v", maybeLog.Err)
				continue Loop
			}
			p.respondToLog(maybeLog.Log)

		case <-p.pollTicker.Tick():
			p.pollIfEligible(p.threshold)

		case <-p.idleTicker:
			p.pollIfEligible(0)
		}
	}
}

func (p *PollingDeviationChecker) determineMostRecentSubmittedRoundID() {
	myAccount, err := p.store.KeyStore.GetFirstAccount()
	if err != nil {
		logger.Error("error determining most recent submitted round ID: ", err)
		return
	}

	// Just to be particularly defensive against issues with the DB or TxManager, we
	// fetch the most recent 5 transactions we've submitted to this aggregator from our
	// Chainlink node address.  Take the highest round ID among them and store it so
	// that we avoid re-polling for a given round when our tx takes a while to confirm.
	txs, err := p.store.ORM.FindTxsBySenderAndRecipient(myAccount.Address, p.initr.InitiatorParams.Address, 0, 5)
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		logger.Error("error determining most recent submitted round ID: ", err)
		return
	}

	// Parse the round IDs from the transaction data
	for _, tx := range txs {
		if len(tx.Data) != 68 {
			logger.Warnw("found Flux Monitor tx with bad data payload",
				"txID", tx.ID,
			)
			continue
		}

		roundIDBytes := tx.Data[4:36]
		roundID := big.NewInt(0).SetBytes(roundIDBytes).Uint64()
		if roundID > p.mostRecentSubmittedRoundID {
			p.mostRecentSubmittedRoundID = roundID
		}
	}
	logger.Infow(fmt.Sprintf("roundID of most recent submission is %v", p.mostRecentSubmittedRoundID),
		"jobID", p.initr.JobSpecID,
		"aggregator", p.initr.InitiatorParams.Address.Hex(),
	)
}

func (p *PollingDeviationChecker) respondToLog(log interface{}) {
	switch log := log.(type) {
	case *contracts.LogNewRound:
		logger.Debugw("NewRound log", p.loggerFieldsForNewRound(log)...)
		p.respondToNewRoundLog(log)

	case *contracts.LogAnswerUpdated:
		logger.Debugw("AnswerUpdated log", p.loggerFieldsForAnswerUpdated(log)...)
		p.respondToAnswerUpdatedLog(log)

	default:
	}
}

// The AnswerUpdated log tells us that round has successfully close with a new
// answer.  This tells us that we need to reset our poll ticker.
//
// Only invoked by the CSP consumer on the single goroutine for thread safety.
func (p *PollingDeviationChecker) respondToAnswerUpdatedLog(log *contracts.LogAnswerUpdated) {
	if p.reportableRoundID != nil && log.RoundId.Cmp(p.reportableRoundID) < 0 {
		// Ignore old rounds
		logger.Debugw("Ignoring stale AnswerUpdated log", p.loggerFieldsForAnswerUpdated(log)...)
		return
	}
	p.pollTicker.Reset()
}

// The NewRound log tells us that an oracle has initiated a new round.  This tells us that we
// need to poll and submit an answer to the contract regardless of the deviation.
//
// Only invoked by the CSP consumer on the single goroutine for thread safety.
func (p *PollingDeviationChecker) respondToNewRoundLog(log *contracts.LogNewRound) {
	// Ignore old rounds
	if p.reportableRoundID != nil && log.RoundId.Cmp(p.reportableRoundID) < 0 {
		logger.Infow("Ignoring new round request: new < current", p.loggerFieldsForNewRound(log)...)
		return
	}

	// The idleThreshold resets when a new round starts
	if p.idleThreshold > 0 {
		p.idleTicker = time.After(p.idleThreshold)
	}

	// Ignore rounds we started
	acct, err := p.store.KeyStore.GetFirstAccount()
	if err != nil {
		logger.Errorw(fmt.Sprintf("error fetching account from keystore: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	} else if log.StartedBy == acct.Address {
		return
	}

	jobSpecID := p.initr.JobSpecID.String()
	promSetBigInt(promFMSeenRound.WithLabelValues(jobSpecID), log.RoundId)

	// It's possible for RoundState() to return a higher round ID than the one in the NewRound log
	// (for example, if a large set of logs are delayed and arrive all at once).  We trust the value
	// from RoundState() over the one in the log, and record it as the current ReportableRoundID.
	roundState, err := p.roundState()
	if err != nil {
		logger.Errorw(fmt.Sprintf("Ignoring new round request: error fetching eligibility from contract: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	}
	p.reportableRoundID = big.NewInt(int64(roundState.ReportableRoundID))

	if !roundState.EligibleToSubmit {
		logger.Infow("Ignoring new round request: not eligible to submit", p.loggerFieldsForNewRound(log)...)
		return
	}

	logger.Infow("Responding to new round request: new > current", p.loggerFieldsForNewRound(log)...)

	polledAnswer, err := p.fetcher.Fetch()
	if err != nil {
		logger.Errorw(fmt.Sprintf("unable to fetch median price: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	}

	p.createJobRun(polledAnswer, p.reportableRoundID)
}

func (p *PollingDeviationChecker) pollIfEligible(threshold float64) (createdJobRun bool) {
	if p.connected.Get() == false {
		logger.Warn("not connected to Ethereum node, skipping poll")
		return false
	}

	roundState, err := p.roundState()
	if err != nil {
		logger.Errorf("unable to determine eligibility to submit from FluxAggregator contract: %v", err)
		return false
	}

	// It's pointless to listen to logs from before the current reporting round
	p.reportableRoundID = big.NewInt(int64(roundState.ReportableRoundID))

	// If we've already submitted an answer for this round, but the tx is still pending, don't resubmit
	if p.mostRecentSubmittedRoundID >= uint64(roundState.ReportableRoundID) {
		logger.Infow(fmt.Sprintf("already submitted for round %v, tx is still pending", roundState.ReportableRoundID),
			"jobID", p.initr.JobSpecID,
		)
		return false
	}

	if !roundState.EligibleToSubmit {
		logger.Infow("not eligible to submit, skipping poll",
			"jobID", p.initr.JobSpecID,
		)
		return false
	}

	available, err := p.fluxAggregator.GetAvailableFunds()
	if err != nil {
		logger.Errorf("unable to determine available funds from FluxAggregator contract : %v", err)
		return false
	}

	if available.Cmp(p.store.Config.MinimumContractPayment()) < 0 {
		logger.Infow("available funds are required to cover Flux Monitor service payments",
			"jobID", p.initr.JobSpecID,
			"availableFunds", available.String(),
		)
		return false
	}

	polledAnswer, err := p.fetcher.Fetch()
	if err != nil {
		logger.Errorf("can't fetch answer: %v", err)
		return false
	}

	jobSpecID := p.initr.JobSpecID.String()
	promSetDecimal(promFMSeenValue.WithLabelValues(jobSpecID), polledAnswer)

	latestAnswer := decimal.NewFromBigInt(roundState.LatestAnswer, -p.precision)
	if !OutsideDeviation(latestAnswer, polledAnswer, threshold) {
		logger.Debugw("deviation < threshold, not submitting",
			"latestAnswer", latestAnswer,
			"polledAnswer", polledAnswer,
			"threshold", threshold,
		)
		return false
	}

	logger.Infow("deviation > threshold, starting new round",
		"reportableRound", roundState.ReportableRoundID,
		"address", p.initr.Address.Hex(),
		"jobID", p.initr.JobSpecID,
	)
	err = p.createJobRun(polledAnswer, p.reportableRoundID)
	if err != nil {
		logger.Errorf("can't create job run: %v", err)
		return false
	}

	promSetDecimal(promFMReportedValue.WithLabelValues(jobSpecID), polledAnswer)
	promSetBigInt(promFMReportedRound.WithLabelValues(jobSpecID), p.reportableRoundID)
	return true
}

func (p *PollingDeviationChecker) roundState() (contracts.FluxAggregatorRoundState, error) {
	acct, err := p.store.KeyStore.GetFirstAccount()
	if err != nil {
		return contracts.FluxAggregatorRoundState{}, err
	}
	return p.fluxAggregator.RoundState(acct.Address)
}

// jobRunRequest is the request used to trigger a Job Run by the Flux Monitor.
type jobRunRequest struct {
	Result           decimal.Decimal `json:"result"`
	Address          string          `json:"address"`
	FunctionSelector string          `json:"functionSelector"`
	DataPrefix       string          `json:"dataPrefix"`
}

func (p *PollingDeviationChecker) createJobRun(polledAnswer decimal.Decimal, nextRound *big.Int) error {
	methodID, err := p.fluxAggregator.GetMethodID("updateAnswer")
	if err != nil {
		return err
	}

	nextRoundData, err := utils.EVMWordBigInt(nextRound)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(jobRunRequest{
		Result:           polledAnswer,
		Address:          p.initr.InitiatorParams.Address.Hex(),
		FunctionSelector: hexutil.Encode(methodID),
		DataPrefix:       hexutil.Encode(nextRoundData),
	})
	if err != nil {
		return errors.Wrapf(err, "unable to encode Job Run request in JSON")
	}
	runData, err := models.ParseJSON(payload)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to start chainlink run with payload %s", payload))
	}
	runRequest := models.NewRunRequest(runData)

	_, err = p.runManager.Create(p.initr.JobSpecID, &p.initr, nil, runRequest)
	if err != nil {
		return err
	}

	p.mostRecentSubmittedRoundID = nextRound.Uint64()

	return nil
}

func (p *PollingDeviationChecker) loggerFieldsForNewRound(log *contracts.LogNewRound) []interface{} {
	return []interface{}{
		"reportableRound", p.reportableRoundID,
		"round", log.RoundId,
		"startedBy", log.StartedBy.Hex(),
		"startedAt", log.StartedAt.String(),
		"contract", log.Address.Hex(),
		"jobID", p.initr.JobSpecID,
	}
}

func (p *PollingDeviationChecker) loggerFieldsForAnswerUpdated(log *contracts.LogAnswerUpdated) []interface{} {
	return []interface{}{
		"round", log.RoundId,
		"answer", log.Current.String(),
		"timestamp", log.Timestamp.String(),
		"contract", log.Address.Hex(),
		"job", p.initr.JobSpecID,
	}
}

// OutsideDeviation checks whether the next price is outside the threshold.
func OutsideDeviation(curAnswer, nextAnswer decimal.Decimal, threshold float64) bool {
	if curAnswer.IsZero() {
		logger.Infow("Current price is 0, deviation automatically met", "answer", decimal.Zero)
		return true
	}

	diff := curAnswer.Sub(nextAnswer).Abs()
	percentage := diff.Div(curAnswer).Mul(decimal.NewFromInt(100))
	if percentage.LessThan(decimal.NewFromFloat(threshold)) {
		logger.Debugw(
			"Deviation threshold not met",
			"difference", percentage,
			"threshold", threshold,
			"currentAnswer", curAnswer,
			"nextAnswer", nextAnswer)
		return false
	}
	logger.Infow(
		"Deviation threshold met",
		"difference", percentage,
		"threshold", threshold,
		"currentAnswer", curAnswer,
		"nextAnswer", nextAnswer,
	)
	return true
}
