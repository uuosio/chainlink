package fluxmonitor_test

import (
	"fmt"
	"math"
	"math/big"
	"net/url"
	"reflect"
	"testing"
	"time"

	"chainlink/core/cmd"
	"chainlink/core/internal/cltest"
	"chainlink/core/internal/mocks"
	"chainlink/core/services/eth"
	"chainlink/core/services/eth/contracts"
	"chainlink/core/services/fluxmonitor"
	"chainlink/core/store"
	"chainlink/core/store/models"
	"chainlink/core/utils"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	updateAnswerHash     = utils.MustHash("updateAnswer(uint256,int256)")
	updateAnswerSelector = updateAnswerHash[:4]
)

func ensureAccount(t *testing.T, store *store.Store) common.Address {
	t.Helper()
	auth := cmd.TerminalKeyStoreAuthenticator{Prompter: &cltest.MockCountingPrompter{T: t}}
	_, err := auth.Authenticate(store, "somepassword")
	assert.NoError(t, err)
	assert.True(t, store.KeyStore.HasAccounts())
	acct, err := store.KeyStore.GetFirstAccount()
	assert.NoError(t, err)
	return acct.Address
}

func TestConcreteFluxMonitor_AddJobRemoveJob(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	txm := new(mocks.TxManager)
	store.TxManager = txm
	txm.On("GetBlockHeight").Return(uint64(123), nil)

	t.Run("starts and stops DeviationCheckers when jobs are added and removed", func(t *testing.T) {
		job := cltest.NewJobWithFluxMonitorInitiator()
		runManager := new(mocks.RunManager)
		started := make(chan struct{}, 1)

		dc := new(mocks.DeviationChecker)
		dc.On("Start", mock.Anything, mock.Anything).Return(nil).Run(func(mock.Arguments) {
			started <- struct{}{}
		})

		checkerFactory := new(mocks.DeviationCheckerFactory)
		checkerFactory.On("New", job.Initiators[0], runManager, store.ORM).Return(dc, nil)
		fm := fluxmonitor.New(store, runManager)
		fluxmonitor.ExportedSetCheckerFactory(fm, checkerFactory)
		require.NoError(t, fm.Start())

		// Add Job
		require.NoError(t, fm.AddJob(job))

		cltest.CallbackOrTimeout(t, "deviation checker started", func() {
			<-started
		})
		checkerFactory.AssertExpectations(t)
		dc.AssertExpectations(t)

		// Remove Job
		removed := make(chan struct{})
		dc.On("Stop").Return().Run(func(mock.Arguments) {
			removed <- struct{}{}
		})
		fm.RemoveJob(job.ID)
		cltest.CallbackOrTimeout(t, "deviation checker stopped", func() {
			<-removed
		})

		fm.Stop()

		dc.AssertExpectations(t)
	})

	t.Run("does not error or attempt to start a DeviationChecker when receiving a non-Flux Monitor job", func(t *testing.T) {
		job := cltest.NewJobWithRunLogInitiator()
		runManager := new(mocks.RunManager)
		checkerFactory := new(mocks.DeviationCheckerFactory)
		fm := fluxmonitor.New(store, runManager)
		fluxmonitor.ExportedSetCheckerFactory(fm, checkerFactory)

		err := fm.Start()
		require.NoError(t, err)
		defer fm.Stop()

		err = fm.AddJob(job)
		require.NoError(t, err)

		checkerFactory.AssertNotCalled(t, "New", mock.Anything, mock.Anything, mock.Anything)
	})
}

func TestPollingDeviationChecker_PollIfEligible(t *testing.T) {
	tests := []struct {
		name                      string
		eligible                  bool
		connected                 bool
		threshold                 float64
		latestAnswer              int64
		polledAnswer              int64
		expectedToFetchRoundState bool
		expectedToPoll            bool
		expectedToSubmit          bool
	}{
		{"eligible, connected, threshold > 0, answers deviate", true, true, 0.1, 1, 100, true, true, true},
		{"eligible, connected, threshold > 0, answers do not deviate", true, true, 0.1, 100, 100, true, true, false},
		{"eligible, connected, threshold == 0, answers deviate", true, true, 0, 1, 100, true, true, true},
		{"eligible, connected, threshold == 0, answers do not deviate", true, true, 0, 1, 100, true, true, true},

		{"eligible, disconnected, threshold > 0, answers deviate", true, false, 0.1, 1, 100, false, false, false},
		{"eligible, disconnected, threshold > 0, answers do not deviate", true, false, 0.1, 100, 100, false, false, false},
		{"eligible, disconnected, threshold == 0, answers deviate", true, false, 0, 1, 100, false, false, false},
		{"eligible, disconnected, threshold == 0, answers do not deviate", true, false, 0, 1, 100, false, false, false},

		{"ineligible, connected, threshold > 0, answers deviate", false, true, 0.1, 1, 100, true, false, false},
		{"ineligible, connected, threshold > 0, answers do not deviate", false, true, 0.1, 100, 100, true, false, false},
		{"ineligible, connected, threshold == 0, answers deviate", false, true, 0, 1, 100, true, false, false},
		{"ineligible, connected, threshold == 0, answers do not deviate", false, true, 0, 1, 100, true, false, false},

		{"ineligible, disconnected, threshold > 0, answers deviate", false, false, 0.1, 1, 100, false, false, false},
		{"ineligible, disconnected, threshold > 0, answers do not deviate", false, false, 0.1, 100, 100, false, false, false},
		{"ineligible, disconnected, threshold == 0, answers deviate", false, false, 0, 1, 100, false, false, false},
		{"ineligible, disconnected, threshold == 0, answers do not deviate", false, false, 0, 1, 100, false, false, false},
	}

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	nodeAddr := ensureAccount(t, store)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rm := new(mocks.RunManager)
			fetcher := new(mocks.Fetcher)
			fluxAggregator := new(mocks.FluxAggregator)

			job := cltest.NewJobWithFluxMonitorInitiator()
			initr := job.Initiators[0]
			initr.ID = 1

			const reportableRoundID = 2
			latestAnswerNoPrecision := test.latestAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision)))

			if test.expectedToFetchRoundState {
				roundState := contracts.FluxAggregatorRoundState{
					ReportableRoundID: reportableRoundID,
					EligibleToSubmit:  test.eligible,
					LatestAnswer:      big.NewInt(latestAnswerNoPrecision),
				}
				fluxAggregator.On("RoundState", nodeAddr).Return(roundState, nil).
					Once()
			}

			if test.expectedToPoll {
				fetcher.On("Fetch").Return(decimal.NewFromInt(test.polledAnswer), nil)
			}

			if test.expectedToSubmit {
				run := cltest.NewJobRun(job)
				data, err := models.ParseJSON([]byte(fmt.Sprintf(`{
					"result": "%d",
					"address": "%s",
					"functionSelector": "0x%x",
					"dataPrefix": "0x000000000000000000000000000000000000000000000000000000000000000%d"
				}`, test.polledAnswer, initr.InitiatorParams.Address.Hex(), updateAnswerSelector, reportableRoundID)))
				require.NoError(t, err)

				rm.On("Create", job.ID, &initr, mock.Anything, mock.MatchedBy(func(runRequest *models.RunRequest) bool {
					return reflect.DeepEqual(runRequest.RequestParams.Result.Value(), data.Result.Value())
				})).Return(&run, nil)

				fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil)
			}

			checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Second)
			require.NoError(t, err)

			if test.connected {
				checker.OnConnect()
			}

			checker.ExportedPollIfEligible(test.threshold)

			fluxAggregator.AssertExpectations(t)
			fetcher.AssertExpectations(t)
			rm.AssertExpectations(t)
		})
	}
}

func TestPollingDeviationChecker_TriggerIdleTimeThreshold(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	nodeAddr := ensureAccount(t, store)

	tests := []struct {
		name             string
		idleThreshold    time.Duration
		expectedToSubmit bool
	}{
		{"no idleThreshold", 0, false},
		{"idleThreshold > 0", 10 * time.Millisecond, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := new(mocks.Fetcher)
			runManager := new(mocks.RunManager)
			fluxAggregator := new(mocks.FluxAggregator)

			job := cltest.NewJobWithFluxMonitorInitiator()
			initr := job.Initiators[0]
			initr.ID = 1
			initr.PollingInterval = models.Duration(math.MaxInt64)
			initr.IdleThreshold = models.Duration(test.idleThreshold)
			jobRun := cltest.NewJobRun(job)

			const fetchedAnswer = 100
			answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision))))

			didSubscribe := make(chan struct{})
			fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(false, eth.UnsubscribeFunc(func() {}), nil).Run(func(mock.Arguments) {
				close(didSubscribe)
			})

			roundState1 := contracts.FluxAggregatorRoundState{ReportableRoundID: 1, EligibleToSubmit: true, LatestAnswer: answerBigInt}
			roundState2 := contracts.FluxAggregatorRoundState{ReportableRoundID: 2, EligibleToSubmit: true, LatestAnswer: answerBigInt}
			roundState3 := contracts.FluxAggregatorRoundState{ReportableRoundID: 3, EligibleToSubmit: true, LatestAnswer: answerBigInt}

			jobRunCreated := make(chan struct{}, 3)

			if test.expectedToSubmit {
				fetcher.On("Fetch").Return(decimal.NewFromInt(fetchedAnswer), nil)

				fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil).Times(3)
				fluxAggregator.On("RoundState", nodeAddr).Return(roundState1, nil).Once()
				fluxAggregator.On("RoundState", nodeAddr).Return(roundState2, nil).Once()
				fluxAggregator.On("RoundState", nodeAddr).Return(roundState3, nil).Once()

				runManager.On("Create", job.ID, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(&jobRun, nil).
					Run(func(args mock.Arguments) {
						jobRunCreated <- struct{}{}
					})
			}

			deviationChecker, err := fluxmonitor.NewPollingDeviationChecker(
				store,
				fluxAggregator,
				initr,
				runManager,
				fetcher,
				time.Duration(math.MaxInt64),
			)
			require.NoError(t, err)

			deviationChecker.Start()
			require.Len(t, jobRunCreated, 0, "no Job Runs created")

			<-didSubscribe
			deviationChecker.OnConnect()

			if test.expectedToSubmit {
				require.Eventually(t, func() bool { return len(jobRunCreated) == 1 }, 3*time.Second, 10*time.Millisecond)
				deviationChecker.HandleLog(&contracts.LogNewRound{RoundId: big.NewInt(int64(roundState1.ReportableRoundID))}, nil)
				require.Eventually(t, func() bool { return len(jobRunCreated) == 2 }, 3*time.Second, 10*time.Millisecond)
				deviationChecker.HandleLog(&contracts.LogNewRound{RoundId: big.NewInt(int64(roundState2.ReportableRoundID))}, nil)
				require.Eventually(t, func() bool { return len(jobRunCreated) == 3 }, 3*time.Second, 10*time.Millisecond)
			}

			deviationChecker.Stop()

			if !test.expectedToSubmit {
				require.Len(t, jobRunCreated, 0)
			}

			fetcher.AssertExpectations(t)
			runManager.AssertExpectations(t)
			fluxAggregator.AssertExpectations(t)
		})
	}
}

func TestPollingDeviationChecker_RespondToNewRound(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	nodeAddr := ensureAccount(t, store)

	type roundIDCase struct {
		name                     string
		storedReportableRoundID  *big.Int
		fetchedReportableRoundID uint32
		logRoundID               int64
	}
	var (
		noStored_fetched_eq_log  = roundIDCase{"(stored = nil) fetched = log", nil, 5, 5}
		noStored_fetched_gt_log  = roundIDCase{"(stored = nil) fetched > log", nil, 5, 10}
		noStored_fetched_lt_log  = roundIDCase{"(stored = nil) fetched < log", nil, 5, 1}
		stored_lt_fetched_lt_log = roundIDCase{"stored < fetched < log", big.NewInt(5), 10, 15}
		stored_lt_log_lt_fetched = roundIDCase{"stored < log < fetched", big.NewInt(5), 15, 10}
		fetched_lt_stored_lt_log = roundIDCase{"fetched < stored < log", big.NewInt(10), 5, 15}
		fetched_lt_log_lt_stored = roundIDCase{"fetched < log < stored", big.NewInt(15), 5, 10}
		log_lt_fetched_lt_stored = roundIDCase{"log < fetched < stored", big.NewInt(15), 10, 5}
		log_lt_stored_lt_fetched = roundIDCase{"log < stored < fetched", big.NewInt(10), 15, 5}
		stored_lt_fetched_eq_log = roundIDCase{"stored < fetched = log", big.NewInt(5), 10, 10}
		stored_eq_fetched_lt_log = roundIDCase{"stored = fetched < log", big.NewInt(5), 5, 10}
		stored_eq_log_lt_fetched = roundIDCase{"stored = log < fetched", big.NewInt(5), 10, 5}
		fetched_lt_stored_eq_log = roundIDCase{"fetched < stored = log", big.NewInt(10), 5, 10}
		fetched_eq_log_lt_stored = roundIDCase{"fetched = log < stored", big.NewInt(10), 5, 5}
		log_lt_fetched_eq_stored = roundIDCase{"log < fetched = stored", big.NewInt(10), 10, 5}
	)

	type answerCase struct {
		name         string
		latestAnswer int64
		polledAnswer int64
	}
	var (
		deviationThresholdExceeded    = answerCase{"deviation", 10, 100}
		deviationThresholdNotExceeded = answerCase{"no deviation", 10, 10}
	)

	tests := []struct {
		eligible      bool
		startedBySelf bool
		roundIDCase
		answerCase
	}{
		{true, true, noStored_fetched_eq_log, deviationThresholdExceeded},
		{true, true, noStored_fetched_gt_log, deviationThresholdExceeded},
		{true, true, noStored_fetched_lt_log, deviationThresholdExceeded},
		{true, true, stored_lt_fetched_lt_log, deviationThresholdExceeded},
		{true, true, stored_lt_log_lt_fetched, deviationThresholdExceeded},
		{true, true, fetched_lt_stored_lt_log, deviationThresholdExceeded},
		{true, true, fetched_lt_log_lt_stored, deviationThresholdExceeded},
		{true, true, log_lt_fetched_lt_stored, deviationThresholdExceeded},
		{true, true, log_lt_stored_lt_fetched, deviationThresholdExceeded},
		{true, true, stored_lt_fetched_eq_log, deviationThresholdExceeded},
		{true, true, stored_eq_fetched_lt_log, deviationThresholdExceeded},
		{true, true, stored_eq_log_lt_fetched, deviationThresholdExceeded},
		{true, true, fetched_lt_stored_eq_log, deviationThresholdExceeded},
		{true, true, fetched_eq_log_lt_stored, deviationThresholdExceeded},
		{true, true, log_lt_fetched_eq_stored, deviationThresholdExceeded},
		{true, true, noStored_fetched_eq_log, deviationThresholdNotExceeded},
		{true, true, noStored_fetched_gt_log, deviationThresholdNotExceeded},
		{true, true, noStored_fetched_lt_log, deviationThresholdNotExceeded},
		{true, true, stored_lt_fetched_lt_log, deviationThresholdNotExceeded},
		{true, true, stored_lt_log_lt_fetched, deviationThresholdNotExceeded},
		{true, true, fetched_lt_stored_lt_log, deviationThresholdNotExceeded},
		{true, true, fetched_lt_log_lt_stored, deviationThresholdNotExceeded},
		{true, true, log_lt_fetched_lt_stored, deviationThresholdNotExceeded},
		{true, true, log_lt_stored_lt_fetched, deviationThresholdNotExceeded},
		{true, true, stored_lt_fetched_eq_log, deviationThresholdNotExceeded},
		{true, true, stored_eq_fetched_lt_log, deviationThresholdNotExceeded},
		{true, true, stored_eq_log_lt_fetched, deviationThresholdNotExceeded},
		{true, true, fetched_lt_stored_eq_log, deviationThresholdNotExceeded},
		{true, true, fetched_eq_log_lt_stored, deviationThresholdNotExceeded},
		{true, true, log_lt_fetched_eq_stored, deviationThresholdNotExceeded},
		{true, false, noStored_fetched_eq_log, deviationThresholdExceeded},
		{true, false, noStored_fetched_gt_log, deviationThresholdExceeded},
		{true, false, noStored_fetched_lt_log, deviationThresholdExceeded},
		{true, false, stored_lt_fetched_lt_log, deviationThresholdExceeded},
		{true, false, stored_lt_log_lt_fetched, deviationThresholdExceeded},
		{true, false, fetched_lt_stored_lt_log, deviationThresholdExceeded},
		{true, false, fetched_lt_log_lt_stored, deviationThresholdExceeded},
		{true, false, log_lt_fetched_lt_stored, deviationThresholdExceeded},
		{true, false, log_lt_stored_lt_fetched, deviationThresholdExceeded},
		{true, false, stored_lt_fetched_eq_log, deviationThresholdExceeded},
		{true, false, stored_eq_fetched_lt_log, deviationThresholdExceeded},
		{true, false, stored_eq_log_lt_fetched, deviationThresholdExceeded},
		{true, false, fetched_lt_stored_eq_log, deviationThresholdExceeded},
		{true, false, fetched_eq_log_lt_stored, deviationThresholdExceeded},
		{true, false, log_lt_fetched_eq_stored, deviationThresholdExceeded},
		{true, false, noStored_fetched_eq_log, deviationThresholdNotExceeded},
		{true, false, noStored_fetched_gt_log, deviationThresholdNotExceeded},
		{true, false, noStored_fetched_lt_log, deviationThresholdNotExceeded},
		{true, false, stored_lt_fetched_lt_log, deviationThresholdNotExceeded},
		{true, false, stored_lt_log_lt_fetched, deviationThresholdNotExceeded},
		{true, false, fetched_lt_stored_lt_log, deviationThresholdNotExceeded},
		{true, false, fetched_lt_log_lt_stored, deviationThresholdNotExceeded},
		{true, false, log_lt_fetched_lt_stored, deviationThresholdNotExceeded},
		{true, false, log_lt_stored_lt_fetched, deviationThresholdNotExceeded},
		{true, false, stored_lt_fetched_eq_log, deviationThresholdNotExceeded},
		{true, false, stored_eq_fetched_lt_log, deviationThresholdNotExceeded},
		{true, false, stored_eq_log_lt_fetched, deviationThresholdNotExceeded},
		{true, false, fetched_lt_stored_eq_log, deviationThresholdNotExceeded},
		{true, false, fetched_eq_log_lt_stored, deviationThresholdNotExceeded},
		{true, false, log_lt_fetched_eq_stored, deviationThresholdNotExceeded},
		{false, true, noStored_fetched_eq_log, deviationThresholdExceeded},
		{false, true, noStored_fetched_gt_log, deviationThresholdExceeded},
		{false, true, noStored_fetched_lt_log, deviationThresholdExceeded},
		{false, true, stored_lt_fetched_lt_log, deviationThresholdExceeded},
		{false, true, stored_lt_log_lt_fetched, deviationThresholdExceeded},
		{false, true, fetched_lt_stored_lt_log, deviationThresholdExceeded},
		{false, true, fetched_lt_log_lt_stored, deviationThresholdExceeded},
		{false, true, log_lt_fetched_lt_stored, deviationThresholdExceeded},
		{false, true, log_lt_stored_lt_fetched, deviationThresholdExceeded},
		{false, true, stored_lt_fetched_eq_log, deviationThresholdExceeded},
		{false, true, stored_eq_fetched_lt_log, deviationThresholdExceeded},
		{false, true, stored_eq_log_lt_fetched, deviationThresholdExceeded},
		{false, true, fetched_lt_stored_eq_log, deviationThresholdExceeded},
		{false, true, fetched_eq_log_lt_stored, deviationThresholdExceeded},
		{false, true, log_lt_fetched_eq_stored, deviationThresholdExceeded},
		{false, true, noStored_fetched_eq_log, deviationThresholdNotExceeded},
		{false, true, noStored_fetched_gt_log, deviationThresholdNotExceeded},
		{false, true, noStored_fetched_lt_log, deviationThresholdNotExceeded},
		{false, true, stored_lt_fetched_lt_log, deviationThresholdNotExceeded},
		{false, true, stored_lt_log_lt_fetched, deviationThresholdNotExceeded},
		{false, true, fetched_lt_stored_lt_log, deviationThresholdNotExceeded},
		{false, true, fetched_lt_log_lt_stored, deviationThresholdNotExceeded},
		{false, true, log_lt_fetched_lt_stored, deviationThresholdNotExceeded},
		{false, true, log_lt_stored_lt_fetched, deviationThresholdNotExceeded},
		{false, true, stored_lt_fetched_eq_log, deviationThresholdNotExceeded},
		{false, true, stored_eq_fetched_lt_log, deviationThresholdNotExceeded},
		{false, true, stored_eq_log_lt_fetched, deviationThresholdNotExceeded},
		{false, true, fetched_lt_stored_eq_log, deviationThresholdNotExceeded},
		{false, true, fetched_eq_log_lt_stored, deviationThresholdNotExceeded},
		{false, true, log_lt_fetched_eq_stored, deviationThresholdNotExceeded},
		{false, false, noStored_fetched_eq_log, deviationThresholdExceeded},
		{false, false, noStored_fetched_gt_log, deviationThresholdExceeded},
		{false, false, noStored_fetched_lt_log, deviationThresholdExceeded},
		{false, false, stored_lt_fetched_lt_log, deviationThresholdExceeded},
		{false, false, stored_lt_log_lt_fetched, deviationThresholdExceeded},
		{false, false, fetched_lt_stored_lt_log, deviationThresholdExceeded},
		{false, false, fetched_lt_log_lt_stored, deviationThresholdExceeded},
		{false, false, log_lt_fetched_lt_stored, deviationThresholdExceeded},
		{false, false, log_lt_stored_lt_fetched, deviationThresholdExceeded},
		{false, false, stored_lt_fetched_eq_log, deviationThresholdExceeded},
		{false, false, stored_eq_fetched_lt_log, deviationThresholdExceeded},
		{false, false, stored_eq_log_lt_fetched, deviationThresholdExceeded},
		{false, false, fetched_lt_stored_eq_log, deviationThresholdExceeded},
		{false, false, fetched_eq_log_lt_stored, deviationThresholdExceeded},
		{false, false, log_lt_fetched_eq_stored, deviationThresholdExceeded},
		{false, false, noStored_fetched_eq_log, deviationThresholdNotExceeded},
		{false, false, noStored_fetched_gt_log, deviationThresholdNotExceeded},
		{false, false, noStored_fetched_lt_log, deviationThresholdNotExceeded},
		{false, false, stored_lt_fetched_lt_log, deviationThresholdNotExceeded},
		{false, false, stored_lt_log_lt_fetched, deviationThresholdNotExceeded},
		{false, false, fetched_lt_stored_lt_log, deviationThresholdNotExceeded},
		{false, false, fetched_lt_log_lt_stored, deviationThresholdNotExceeded},
		{false, false, log_lt_fetched_lt_stored, deviationThresholdNotExceeded},
		{false, false, log_lt_stored_lt_fetched, deviationThresholdNotExceeded},
		{false, false, stored_lt_fetched_eq_log, deviationThresholdNotExceeded},
		{false, false, stored_eq_fetched_lt_log, deviationThresholdNotExceeded},
		{false, false, stored_eq_log_lt_fetched, deviationThresholdNotExceeded},
		{false, false, fetched_lt_stored_eq_log, deviationThresholdNotExceeded},
		{false, false, fetched_eq_log_lt_stored, deviationThresholdNotExceeded},
		{false, false, log_lt_fetched_eq_stored, deviationThresholdNotExceeded},
	}

	for _, test := range tests {
		name := test.answerCase.name + ", " + test.roundIDCase.name
		if test.eligible {
			name += ", eligible"
		} else {
			name += ", ineligible"
		}
		t.Run(name, func(t *testing.T) {
			expectedToFetchRoundState := !test.startedBySelf &&
				(test.storedReportableRoundID == nil || test.logRoundID >= test.storedReportableRoundID.Int64())
			expectedToPoll := test.eligible && expectedToFetchRoundState
			expectedToSubmit := expectedToPoll

			job := cltest.NewJobWithFluxMonitorInitiator()
			initr := job.Initiators[0]
			initr.ID = 1
			initr.InitiatorParams.PollingInterval = models.Duration(1 * time.Hour)

			rm := new(mocks.RunManager)
			fetcher := new(mocks.Fetcher)
			fluxAggregator := new(mocks.FluxAggregator)

			didSubscribe := make(chan struct{})
			fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(false, eth.UnsubscribeFunc(func() {}), nil).Run(func(mock.Arguments) {
				close(didSubscribe)
			})

			if expectedToFetchRoundState {
				fluxAggregator.On("RoundState", nodeAddr).Return(contracts.FluxAggregatorRoundState{
					ReportableRoundID: test.fetchedReportableRoundID,
					LatestAnswer:      big.NewInt(test.latestAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision)))),
					EligibleToSubmit:  test.eligible,
				}, nil)
			}

			if expectedToPoll {
				fetcher.On("Fetch").Return(decimal.NewFromInt(test.polledAnswer), nil)
			}

			if expectedToSubmit {
				fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil)

				data, err := models.ParseJSON([]byte(fmt.Sprintf(`{
					"result": "%d",
					"address": "%s",
					"functionSelector": "0xe6330cf7",
					"dataPrefix": "0x%0x"
				}`, test.polledAnswer, initr.InitiatorParams.Address.Hex(), utils.EVMWordUint64(uint64(test.fetchedReportableRoundID)))))
				require.NoError(t, err)

				rm.On("Create", mock.Anything, mock.Anything, mock.Anything, mock.MatchedBy(func(runRequest *models.RunRequest) bool {
					return reflect.DeepEqual(runRequest.RequestParams.Result.Value(), data.Result.Value())
				})).Return(nil, nil)
			}

			checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Hour)
			require.NoError(t, err)

			checker.ExportedSetStoredReportableRoundID(test.storedReportableRoundID)

			checker.Start()
			<-didSubscribe
			checker.OnConnect()

			var startedBy common.Address
			if test.startedBySelf {
				startedBy = nodeAddr
			}
			checker.HandleLog(&contracts.LogNewRound{RoundId: big.NewInt(test.logRoundID), StartedBy: startedBy}, nil)
			checker.Stop()

			fluxAggregator.AssertExpectations(t)
			fetcher.AssertExpectations(t)
			rm.AssertExpectations(t)
		})
	}
}

func TestOutsideDeviation(t *testing.T) {
	tests := []struct {
		name                string
		curPrice, nextPrice decimal.Decimal
		threshold           float64 // in percentage
		expectation         bool
	}{
		{"0 current price", decimal.NewFromInt(0), decimal.NewFromInt(100), 2, true},
		{"inside deviation", decimal.NewFromInt(100), decimal.NewFromInt(101), 2, false},
		{"equal to deviation", decimal.NewFromInt(100), decimal.NewFromInt(102), 2, true},
		{"outside deviation", decimal.NewFromInt(100), decimal.NewFromInt(103), 2, true},
		{"outside deviation zero", decimal.NewFromInt(100), decimal.NewFromInt(0), 2, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := fluxmonitor.OutsideDeviation(test.curPrice, test.nextPrice, test.threshold)
			assert.Equal(t, test.expectation, actual)
		})
	}
}

func TestExtractFeedURLs(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	bridge := &models.BridgeType{
		Name: models.MustNewTaskType("testbridge"),
		URL:  cltest.WebURL(t, "https://testing.com/bridges"),
	}
	require.NoError(t, store.CreateBridgeType(bridge))

	tests := []struct {
		name        string
		in          string
		expectation []string
	}{
		{
			"single",
			`["https://lambda.staging.devnet.tools/bnc/call"]`,
			[]string{"https://lambda.staging.devnet.tools/bnc/call"},
		},
		{
			"double",
			`["https://lambda.staging.devnet.tools/bnc/call", "https://lambda.staging.devnet.tools/cc/call"]`,
			[]string{"https://lambda.staging.devnet.tools/bnc/call", "https://lambda.staging.devnet.tools/cc/call"},
		},
		{
			"bridge",
			`[{"bridge":"testbridge"}]`,
			[]string{"https://testing.com/bridges"},
		},
		{
			"mixed",
			`["https://lambda.staging.devnet.tools/bnc/call", {"bridge": "testbridge"}]`,
			[]string{"https://lambda.staging.devnet.tools/bnc/call", "https://testing.com/bridges"},
		},
		{
			"empty",
			`[]`,
			[]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initiatorParams := models.InitiatorParams{
				Feeds: cltest.JSONFromString(t, test.in),
			}
			var expectation []*url.URL
			for _, urlString := range test.expectation {
				expectation = append(expectation, cltest.MustParseURL(urlString))
			}
			val, err := fluxmonitor.ExtractFeedURLs(initiatorParams.Feeds, store.ORM)
			require.NoError(t, err)
			assert.Equal(t, val, expectation)
		})
	}
}
