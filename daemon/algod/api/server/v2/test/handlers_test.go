// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklearray"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated"
	generatedV2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/account"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/node"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/execpool"
	"github.com/algorand/go-codec/codec"
)

func setupTestForMethodGet(t *testing.T) (v2.Handlers, echo.Context, *httptest.ResponseRecorder, []account.Root, []transactions.SignedTxn, func()) {
	numAccounts := 1
	numTransactions := 1
	offlineAccounts := true
	mockLedger, rootkeys, _, stxns, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	dummyShutdownChan := make(chan struct{})
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return handler, c, rec, rootkeys, stxns, releasefunc
}

func TestSimpleMockBuilding(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	handler, _, _, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	require.Equal(t, t.Name(), handler.Node.GenesisID())
}

func accountInformationTest(t *testing.T, address string, expectedCode int) {
	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	err := handler.AccountInformation(c, address, generatedV2.AccountInformationParams{})
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
	if address == poolAddr.String() {
		expectedResponse := poolAddrResponseGolden
		actualResponse := generatedV2.AccountResponse{}
		err = protocol.DecodeJSON(rec.Body.Bytes(), &actualResponse)
		require.NoError(t, err)
		require.Equal(t, expectedResponse, actualResponse)
	}
}

func TestAccountInformation(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	accountInformationTest(t, poolAddr.String(), 200)
	accountInformationTest(t, "bad account", 400)
}

func getBlockTest(t *testing.T, blockNum uint64, format string, expectedCode int) {
	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	err := handler.GetBlock(c, blockNum, generatedV2.GetBlockParams{Format: &format})
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
}

func TestGetBlock(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	getBlockTest(t, 0, "json", 200)
	getBlockTest(t, 0, "msgpack", 200)
	getBlockTest(t, 1, "json", 500)
	getBlockTest(t, 0, "bad format", 400)
}

func addBlockHelper(t *testing.T) (v2.Handlers, echo.Context, *httptest.ResponseRecorder, transactions.SignedTxn, func()) {
	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)

	l := handler.Node.LedgerForAPI()

	genBlk, err := l.Block(0)
	require.NoError(t, err)

	// make an app call txn with eval delta
	lsig := transactions.LogicSig{Logic: retOneProgram} // int 1
	program := logic.Program(lsig.Logic)
	lhash := crypto.HashObj(&program)
	var sender basics.Address
	copy(sender[:], lhash[:])
	stx := transactions.SignedTxn{
		Txn: transactions.Transaction{
			Type: protocol.ApplicationCallTx,
			Header: transactions.Header{
				Sender:      sender,
				Fee:         basics.MicroAlgos{Raw: 1000},
				GenesisID:   genBlk.GenesisID(),
				GenesisHash: genBlk.GenesisHash(),
				FirstValid:  1,
				LastValid:   10,
			},
			ApplicationCallTxnFields: transactions.ApplicationCallTxnFields{
				ApplicationID: 1,
				OnCompletion:  transactions.ClearStateOC,
			},
		},
		Lsig: lsig,
	}
	ad := transactions.ApplyData{
		EvalDelta: transactions.EvalDelta{
			LocalDeltas: map[uint64]basics.StateDelta{
				1: {"key": basics.ValueDelta{Action: 1}},
			},
		},
	}

	// put it into a block
	backlogPool := execpool.MakeBacklog(nil, 0, execpool.LowPriority, nil)
	defer backlogPool.Shutdown()

	totalsRound, totals, err := l.LatestTotals()
	require.NoError(t, err)
	require.Equal(t, l.Latest(), totalsRound)
	totalRewardUnits := totals.RewardUnits()
	poolBal, _, _, err := l.LookupLatest(poolAddr)
	require.NoError(t, err)

	var blk bookkeeping.Block
	blk.BlockHeader = bookkeeping.BlockHeader{
		GenesisID:    genBlk.GenesisID(),
		GenesisHash:  genBlk.GenesisHash(),
		Round:        l.Latest() + 1,
		Branch:       genBlk.Hash(),
		TimeStamp:    0,
		RewardsState: genBlk.NextRewardsState(l.Latest()+1, proto, poolBal.MicroAlgos, totalRewardUnits, logging.Base()),
		UpgradeState: genBlk.UpgradeState,
	}

	blk.BlockHeader.TxnCounter = genBlk.TxnCounter

	blk.RewardsPool = genBlk.RewardsPool
	blk.FeeSink = genBlk.FeeSink
	blk.CurrentProtocol = genBlk.CurrentProtocol
	blk.TimeStamp = genBlk.TimeStamp + 1

	txib, err := blk.EncodeSignedTxn(stx, ad)
	blk.Payset = append(blk.Payset, txib)
	blk.BlockHeader.TxnCounter++
	blk.TxnCommitments, err = blk.PaysetCommit()
	require.NoError(t, err)

	err = l.(*data.Ledger).AddBlock(blk, agreement.Certificate{})
	require.NoError(t, err)

	return handler, c, rec, stx, releasefunc
}

func TestGetBlockJsonEncoding(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	handler, c, rec, _, releasefunc := addBlockHelper(t)
	defer releasefunc()

	// fetch the block and ensure it can be properly decoded with the standard JSON decoder
	format := "json"
	err := handler.GetBlock(c, 1, generatedV2.GetBlockParams{Format: &format})
	require.NoError(t, err)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.Bytes()

	response := struct {
		Block bookkeeping.Block `codec:"block"`
	}{}

	err = json.Unmarshal(body, &response)
	require.NoError(t, err)
}

func TestGetSupply(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	handler, c, _, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	err := handler.GetSupply(c)
	require.NoError(t, err)
}

func TestGetStatus(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	err := handler.GetStatus(c)
	require.NoError(t, err)
	stat := cannedStatusReportGolden
	expectedResult := generatedV2.NodeStatusResponse{
		LastRound:                   uint64(stat.LastRound),
		LastVersion:                 string(stat.LastVersion),
		NextVersion:                 string(stat.NextVersion),
		NextVersionRound:            uint64(stat.NextVersionRound),
		NextVersionSupported:        stat.NextVersionSupported,
		TimeSinceLastRound:          uint64(stat.TimeSinceLastRound().Nanoseconds()),
		CatchupTime:                 uint64(stat.CatchupTime.Nanoseconds()),
		StoppedAtUnsupportedRound:   stat.StoppedAtUnsupportedRound,
		LastCatchpoint:              &stat.LastCatchpoint,
		Catchpoint:                  &stat.Catchpoint,
		CatchpointTotalAccounts:     &stat.CatchpointCatchupTotalAccounts,
		CatchpointProcessedAccounts: &stat.CatchpointCatchupProcessedAccounts,
		CatchpointVerifiedAccounts:  &stat.CatchpointCatchupVerifiedAccounts,
		CatchpointTotalBlocks:       &stat.CatchpointCatchupTotalBlocks,
		CatchpointAcquiredBlocks:    &stat.CatchpointCatchupAcquiredBlocks,
	}
	actualResult := generatedV2.NodeStatusResponse{}
	err = protocol.DecodeJSON(rec.Body.Bytes(), &actualResult)
	require.NoError(t, err)
	require.Equal(t, expectedResult, actualResult)
}

func TestGetStatusAfterBlock(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	err := handler.WaitForBlock(c, 0)
	require.NoError(t, err)
	// Expect 400 - the test ledger will always cause "errRequestedRoundInUnsupportedRound",
	// as it has not participated in agreement to build blockheaders
	require.Equal(t, 400, rec.Code)
}

func TestGetTransactionParams(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	err := handler.TransactionParams(c)
	require.NoError(t, err)
	require.Equal(t, 200, rec.Code)
}

func pendingTransactionInformationTest(t *testing.T, txidToUse int, format string, expectedCode int) {
	handler, c, rec, _, stxns, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	txid := "bad txid"
	if txidToUse >= 0 {
		txid = stxns[txidToUse].ID().String()
	}
	params := generatedV2.PendingTransactionInformationParams{Format: &format}
	err := handler.PendingTransactionInformation(c, txid, params)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
}

func TestPendingTransactionInformation(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	pendingTransactionInformationTest(t, 0, "json", 200)
	pendingTransactionInformationTest(t, 0, "msgpack", 200)
	pendingTransactionInformationTest(t, -1, "json", 400)
	pendingTransactionInformationTest(t, 0, "bad format", 400)
}

func getPendingTransactionsTest(t *testing.T, format string, max uint64, expectedCode int) {
	handler, c, rec, _, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	params := generatedV2.GetPendingTransactionsParams{Format: &format, Max: &max}
	err := handler.GetPendingTransactions(c, params)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
	if format == "json" && rec.Code == 200 {
		var response generatedV2.PendingTransactionsResponse

		data := rec.Body.Bytes()
		err = protocol.DecodeJSON(data, &response)
		require.NoError(t, err, string(data))

		if max == 0 || max >= uint64(len(txnPoolGolden)) {
			// all pending txns should be returned
			require.Equal(t, uint64(len(response.TopTransactions)), uint64(len(txnPoolGolden)))
		} else {
			// only max txns should be returned
			require.Equal(t, uint64(len(response.TopTransactions)), max)
		}

		require.Equal(t, response.TotalTransactions, uint64(len(txnPoolGolden)))
		require.GreaterOrEqual(t, response.TotalTransactions, uint64(len(response.TopTransactions)))
	}
}

func TestPendingTransactionLogsEncoding(t *testing.T) {
	partitiontest.PartitionTest(t)

	response := generated.PendingTransactionResponse{
		Logs: &[][]byte{
			{},
			[]byte(string("a")),
			[]byte(string("test")),
			{0},
			{0, 1, 2},
		},
	}

	// log messages should be base64 encoded
	expected := `{
  "logs": [
    "",
    "YQ==",
    "dGVzdA==",
    "AA==",
    "AAEC"
  ],
  "pool-error": "",
  "txn": null
}`

	for _, handle := range []codec.Handle{protocol.JSONHandle, protocol.JSONStrictHandle} {
		var output []byte
		enc := codec.NewEncoderBytes(&output, handle)

		err := enc.Encode(response)
		require.NoError(t, err)

		require.Equal(t, expected, string(output))
	}
}

func TestPendingTransactions(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	getPendingTransactionsTest(t, "json", 0, 200)
	getPendingTransactionsTest(t, "json", 1, 200)
	getPendingTransactionsTest(t, "json", 2, 200)
	getPendingTransactionsTest(t, "json", 3, 200)
	getPendingTransactionsTest(t, "msgpack", 0, 200)
	getPendingTransactionsTest(t, "msgpack", 1, 200)
	getPendingTransactionsTest(t, "msgpack", 2, 200)
	getPendingTransactionsTest(t, "msgpack", 3, 200)
	getPendingTransactionsTest(t, "bad format", 0, 400)
}

func pendingTransactionsByAddressTest(t *testing.T, rootkeyToUse int, format string, expectedCode int) {
	handler, c, rec, rootkeys, _, releasefunc := setupTestForMethodGet(t)
	defer releasefunc()
	address := "bad address"
	if rootkeyToUse >= 0 {
		address = rootkeys[rootkeyToUse].Address().String()
	}
	params := generatedV2.GetPendingTransactionsByAddressParams{Format: &format}
	err := handler.GetPendingTransactionsByAddress(c, address, params)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
}

func TestPendingTransactionsByAddress(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	pendingTransactionsByAddressTest(t, 0, "json", 200)
	pendingTransactionsByAddressTest(t, 0, "msgpack", 200)
	pendingTransactionsByAddressTest(t, 0, "bad format", 400)
	pendingTransactionsByAddressTest(t, -1, "json", 400)
}

func postTransactionTest(t *testing.T, txnToUse, expectedCode int) {
	numAccounts := 5
	numTransactions := 5
	offlineAccounts := true
	mockLedger, _, _, stxns, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	defer releasefunc()
	dummyShutdownChan := make(chan struct{})
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}
	e := echo.New()
	var body io.Reader
	if txnToUse >= 0 {
		stxn := stxns[txnToUse]
		bodyBytes := protocol.Encode(&stxn)
		body = bytes.NewReader(bodyBytes)
	}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler.RawTransaction(c)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
}

func TestPostTransaction(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	postTransactionTest(t, -1, 400)
	postTransactionTest(t, 0, 200)
}

func startCatchupTest(t *testing.T, catchpoint string, nodeError error, expectedCode int) {
	numAccounts := 1
	numTransactions := 1
	offlineAccounts := true
	mockLedger, _, _, _, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	defer releasefunc()
	dummyShutdownChan := make(chan struct{})
	mockNode := makeMockNode(mockLedger, t.Name(), nodeError)
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler.StartCatchup(c, catchpoint)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
}

func TestStartCatchup(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	goodCatchPoint := "5894690#DVFRZUYHEFKRLK5N6DNJRR4IABEVN2D6H76F3ZSEPIE6MKXMQWQA"
	startCatchupTest(t, goodCatchPoint, nil, 201)

	inProgressError := node.MakeCatchpointAlreadyInProgressError("catchpoint")
	startCatchupTest(t, goodCatchPoint, inProgressError, 200)

	unableToStartError := node.MakeCatchpointUnableToStartError("running", "requested")
	startCatchupTest(t, goodCatchPoint, unableToStartError, 400)

	startCatchupTest(t, goodCatchPoint, errors.New("anothing else is internal"), 500)

	badCatchPoint := "bad catchpoint"
	startCatchupTest(t, badCatchPoint, nil, 400)
}

func abortCatchupTest(t *testing.T, catchpoint string, expectedCode int) {
	numAccounts := 1
	numTransactions := 1
	offlineAccounts := true
	mockLedger, _, _, _, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	defer releasefunc()
	dummyShutdownChan := make(chan struct{})
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler.AbortCatchup(c, catchpoint)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)
}

func TestAbortCatchup(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	goodCatchPoint := "5894690#DVFRZUYHEFKRLK5N6DNJRR4IABEVN2D6H76F3ZSEPIE6MKXMQWQA"
	abortCatchupTest(t, goodCatchPoint, 200)
	badCatchPoint := "bad catchpoint"
	abortCatchupTest(t, badCatchPoint, 400)
}

func tealCompileTest(t *testing.T, bytesToUse []byte, expectedCode int,
	enableDeveloperAPI bool, params generated.TealCompileParams,
	expectedSourcemap *logic.SourceMap,
) (response v2.CompileResponseWithSourceMap) {
	numAccounts := 1
	numTransactions := 1
	offlineAccounts := true
	mockLedger, _, _, _, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	defer releasefunc()
	dummyShutdownChan := make(chan struct{})
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	mockNode.config.EnableDeveloperAPI = enableDeveloperAPI
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bytesToUse))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler.TealCompile(c, params)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)

	// Check compiled response.
	if rec.Code == 200 {
		data := rec.Body.Bytes()
		err = protocol.DecodeJSON(data, &response)
		require.NoError(t, err, string(data))
		if expectedSourcemap != nil {
			require.Equal(t, *expectedSourcemap, *response.Sourcemap)
		} else {
			require.Nil(t, response.Sourcemap)
		}
	}
	return
}

func TestTealCompile(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	params := generated.TealCompileParams{}
	tealCompileTest(t, nil, 200, true, params, nil) // nil program should work

	goodProgram := fmt.Sprintf(`#pragma version %d
int 1
assert
int 1`, logic.AssemblerMaxVersion)
	ops, _ := logic.AssembleString(goodProgram)
	expectedSourcemap := logic.GetSourceMap([]string{}, ops.OffsetToLine)
	goodProgramBytes := []byte(goodProgram)

	// Test good program with params
	tealCompileTest(t, goodProgramBytes, 200, true, params, nil)
	paramValue := true
	params = generated.TealCompileParams{Sourcemap: &paramValue}
	tealCompileTest(t, goodProgramBytes, 200, true, params, &expectedSourcemap)
	paramValue = false
	params = generated.TealCompileParams{Sourcemap: &paramValue}
	tealCompileTest(t, goodProgramBytes, 200, true, params, nil)

	// Test a program without the developer API flag.
	tealCompileTest(t, goodProgramBytes, 404, false, params, nil)

	// Test bad program.
	badProgram := "bad program"
	badProgramBytes := []byte(badProgram)
	tealCompileTest(t, badProgramBytes, 400, true, params, nil)
}

func tealDisassembleTest(t *testing.T, program []byte, expectedCode int,
	expectedString string, enableDeveloperAPI bool,
) (response generatedV2.DisassembleResponse) {
	numAccounts := 1
	numTransactions := 1
	offlineAccounts := true
	mockLedger, _, _, _, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	defer releasefunc()
	dummyShutdownChan := make(chan struct{})
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	mockNode.config.EnableDeveloperAPI = enableDeveloperAPI
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(program))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler.TealDisassemble(c)
	require.NoError(t, err)
	require.Equal(t, expectedCode, rec.Code)

	if rec.Code == 200 {
		data := rec.Body.Bytes()
		err = protocol.DecodeJSON(data, &response)
		require.NoError(t, err, string(data))
		require.Equal(t, expectedString, response.Result)
	} else if rec.Code == 400 {
		var response generatedV2.ErrorResponse
		data := rec.Body.Bytes()
		err = protocol.DecodeJSON(data, &response)
		require.NoError(t, err, string(data))
		require.Contains(t, response.Message, expectedString)
	}
	return
}

func TestTealDisassemble(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	// nil program works, but results in invalid version text.
	testProgram := []byte{}
	tealDisassembleTest(t, testProgram, 200, "// invalid version\n", true)

	// Test a valid program.
	for ver := 1; ver <= logic.AssemblerMaxVersion; ver++ {
		goodProgram := `int 1`
		ops, _ := logic.AssembleStringWithVersion(goodProgram, uint64(ver))
		disassembledProgram, _ := logic.Disassemble(ops.Program)
		tealDisassembleTest(t, ops.Program, 200, disassembledProgram, true)
	}
	// Test a nil program without the developer API flag.
	tealDisassembleTest(t, testProgram, 404, "", false)

	// Test bad program.
	badProgram := []byte{1, 99}
	tealDisassembleTest(t, badProgram, 400, "invalid opcode", true)
}

func tealDryrunTest(
	t *testing.T, obj *generatedV2.DryrunRequest, format string,
	expCode int, expResult string, enableDeveloperAPI bool,
) (response generatedV2.DryrunResponse) {
	numAccounts := 1
	numTransactions := 1
	offlineAccounts := true
	mockLedger, _, _, _, releasefunc := testingenv(t, numAccounts, numTransactions, offlineAccounts)
	defer releasefunc()
	dummyShutdownChan := make(chan struct{})
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	mockNode.config.EnableDeveloperAPI = enableDeveloperAPI
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: dummyShutdownChan,
	}

	var data []byte
	if format == "json" {
		data = protocol.EncodeJSON(obj)
	} else {
		obj2, err := v2.DryrunRequestFromGenerated(obj)
		require.NoError(t, err)
		data = protocol.EncodeReflect(&obj2)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler.TealDryrun(c)
	require.NoError(t, err)
	require.Equal(t, expCode, rec.Code)
	if rec.Code == 200 {
		data = rec.Body.Bytes()
		err = protocol.DecodeJSON(data, &response)
		require.NoError(t, err, string(data))

		require.GreaterOrEqual(t, len(response.Txns), 1)
		require.NotNil(t, response.Txns[0].AppCallMessages)
		messages := *response.Txns[0].AppCallMessages
		require.GreaterOrEqual(t, len(messages), 1)
		require.Equal(t, expResult, messages[len(messages)-1])
	}
	return
}

func TestTealDryrun(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var gdr generated.DryrunRequest
	txns := []transactions.SignedTxn{
		{
			Txn: transactions.Transaction{
				Type: protocol.ApplicationCallTx,
				ApplicationCallTxnFields: transactions.ApplicationCallTxnFields{
					ApplicationID:   1,
					ApprovalProgram: []byte{1, 2, 3},
					ApplicationArgs: [][]byte{
						[]byte("check"),
						[]byte("bar"),
					},
				},
			},
		},
	}
	for i := range txns {
		enc := protocol.EncodeJSON(&txns[i])
		gdr.Txns = append(gdr.Txns, enc)
	}

	sucOps, err := logic.AssembleStringWithVersion("int 1", 2)
	require.NoError(t, err)

	failOps, err := logic.AssembleStringWithVersion("int 0", 2)
	require.NoError(t, err)

	gdr.Apps = []generated.Application{
		{
			Id: 1,
			Params: generated.ApplicationParams{
				ApprovalProgram: sucOps.Program,
			},
		},
	}
	localv := make(generated.TealKeyValueStore, 1)
	localv[0] = generated.TealKeyValue{
		Key:   "foo",
		Value: generated.TealValue{Type: uint64(basics.TealBytesType), Bytes: "bar"},
	}

	gdr.Accounts = []generated.Account{
		{
			Address: basics.Address{}.String(),
			AppsLocalState: &[]generated.ApplicationLocalState{{
				Id:       1,
				KeyValue: &localv,
			}},
		},
	}

	gdr.ProtocolVersion = string(protocol.ConsensusFuture)
	tealDryrunTest(t, &gdr, "json", 200, "PASS", true)
	tealDryrunTest(t, &gdr, "msgp", 200, "PASS", true)
	tealDryrunTest(t, &gdr, "msgp", 404, "", false)

	gdr.ProtocolVersion = "unk"
	tealDryrunTest(t, &gdr, "json", 400, "", true)
	gdr.ProtocolVersion = ""

	ddr := tealDryrunTest(t, &gdr, "json", 200, "PASS", true)
	require.Equal(t, string(protocol.ConsensusCurrentVersion), ddr.ProtocolVersion)
	gdr.ProtocolVersion = string(protocol.ConsensusFuture)
	ddr = tealDryrunTest(t, &gdr, "json", 200, "PASS", true)
	require.Equal(t, string(protocol.ConsensusFuture), ddr.ProtocolVersion)

	gdr.Apps[0].Params.ApprovalProgram = failOps.Program
	tealDryrunTest(t, &gdr, "json", 200, "REJECT", true)
	tealDryrunTest(t, &gdr, "msgp", 200, "REJECT", true)
	tealDryrunTest(t, &gdr, "json", 404, "", false)
}

func TestAppendParticipationKeys(t *testing.T) {
	partitiontest.PartitionTest(t)

	mockLedger, _, _, _, releasefunc := testingenv(t, 1, 1, true)
	defer releasefunc()
	mockNode := makeMockNode(mockLedger, t.Name(), nil)
	handler := v2.Handlers{
		Node:     mockNode,
		Log:      logging.Base(),
		Shutdown: make(chan struct{}),
	}

	id := account.ParticipationID{}
	id[0] = 10

	t.Run("Happy path", func(t *testing.T) {
		// Create test object to append.
		keys := make(account.StateProofKeys, 2)
		testKey1 := crypto.FalconSigner{}
		testKey1.PrivateKey[0] = 100

		testKey2 := crypto.FalconSigner{}
		testKey2.PrivateKey[0] = 101

		keys[0] = merklesignature.KeyRoundPair{Round: 100, Key: &testKey1}
		keys[1] = merklesignature.KeyRoundPair{Round: 101, Key: &testKey2}
		keyBytes := protocol.Encode(keys)

		// Put keys in the body.
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(keyBytes))
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Call handler with request.
		err := handler.AppendKeys(c, id.String())

		// Verify that request was properly received and deserialized.
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, id, mockNode.id)
		require.Len(t, mockNode.keys, 2)
		require.Equal(t, mockNode.keys[0].Round, keys[0].Round)
		require.Equal(t, mockNode.keys[0].Key, keys[0].Key)

		require.Equal(t, mockNode.keys[1].Round, keys[1].Round)
		require.Equal(t, mockNode.keys[1].Key, keys[1].Key)

	})

	t.Run("Invalid body", func(t *testing.T) {
		// Create request with bogus bytes in the body
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte{0x99, 0x88, 0x77}))
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Call handler with request.
		err := handler.AppendKeys(c, id.String())

		// Verify that request was properly received and deserialized.
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "unable to parse keys from body: msgpack decode error")
	})

	t.Run("Empty body", func(t *testing.T) {
		// Create test object with no keys to append.
		keys := make(account.StateProofKeys, 0)
		keyBytes := protocol.Encode(keys)

		// Put keys in the body.
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(keyBytes))
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Call handler with request.
		err := handler.AppendKeys(c, id.String())

		// Verify that request was properly received and deserialized.
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "empty request, please attach keys to request body")
	})

	t.Run("Internal error", func(t *testing.T) {
		// Create mock node with an error.
		expectedErr := errors.New("expected error")
		mockNode := makeMockNode(mockLedger, t.Name(), expectedErr)
		handler := v2.Handlers{
			Node:     mockNode,
			Log:      logging.Base(),
			Shutdown: make(chan struct{}),
		}

		keys := make(account.StateProofKeys, 2)
		testKey1 := crypto.FalconSigner{}
		testKey1.PrivateKey[0] = 100

		testKey2 := crypto.FalconSigner{}
		testKey2.PrivateKey[0] = 101

		keys[0] = merklesignature.KeyRoundPair{Round: 100, Key: &testKey1}
		keys[1] = merklesignature.KeyRoundPair{Round: 101, Key: &testKey2}
		keyBytes := protocol.Encode(keys)

		// Put keys in the body.
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(keyBytes))
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Call handler with request.
		err := handler.AppendKeys(c, id.String())

		// Verify that request was properly received and deserialized.
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Contains(t, rec.Body.String(), expectedErr.Error())
	})
}

// TxnMerkleElemRaw this struct helps creates a hashable struct from the bytes
type TxnMerkleElemRaw struct {
	Txn  crypto.Digest // txn id
	Stib crypto.Digest // hash value of transactions.SignedTxnInBlock
}

func txnMerkleToRaw(txid [crypto.DigestSize]byte, stib [crypto.DigestSize]byte) (buf []byte) {
	buf = make([]byte, 2*crypto.DigestSize)
	copy(buf[:], txid[:])
	copy(buf[crypto.DigestSize:], stib[:])
	return
}

// ToBeHashed implements the crypto.Hashable interface.
func (tme *TxnMerkleElemRaw) ToBeHashed() (protocol.HashID, []byte) {
	return protocol.TxnMerkleLeaf, txnMerkleToRaw(tme.Txn, tme.Stib)
}

func TestGetProofDefault(t *testing.T) {
	partitiontest.PartitionTest(t)
	a := assert.New(t)

	handler, c, rec, stx, releasefunc := addBlockHelper(t)
	defer releasefunc()

	txid := stx.ID()
	err := handler.GetProof(c, 1, txid.String(), generated.GetProofParams{})
	a.NoError(err)

	var resp generatedV2.ProofResponse
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	a.NoError(err)
	a.Equal("sha512_256", resp.Hashtype)

	l := handler.Node.LedgerForAPI()
	blkHdr, err := l.BlockHdr(1)
	a.NoError(err)

	// Build merklearray.Proof from ProofResponse
	var proof merklearray.Proof
	proof.HashFactory = crypto.HashFactory{HashType: crypto.Sha512_256}
	proof.TreeDepth = uint8(resp.Treedepth)
	a.NotEqual(proof.TreeDepth, 0)
	proofconcat := resp.Proof
	for len(proofconcat) > 0 {
		var d crypto.Digest
		copy(d[:], proofconcat)
		proof.Path = append(proof.Path, d[:])
		proofconcat = proofconcat[len(d):]
	}

	element := TxnMerkleElemRaw{Txn: crypto.Digest(txid)}
	copy(element.Stib[:], resp.Stibhash[:])
	elems := make(map[uint64]crypto.Hashable)
	elems[0] = &element

	// Verifies that the default proof is using SHA512_256
	err = merklearray.Verify(blkHdr.TxnCommitments.NativeSha512_256Commitment.ToSlice(), elems, &proof)
	a.NoError(err)
}
